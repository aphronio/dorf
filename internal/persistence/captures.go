package persistence

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	CaptureLifetime    = 3 * time.Minute
	CapturePinLifetime = 30 * time.Second
	captureRetention   = 5 * time.Minute
	maxCaptures        = 32
)

var (
	ErrCaptureNotFound = errors.New("capture attempt is unavailable")
	ErrCaptureConflict = errors.New("capture attempt cannot accept this operation")
)

// CaptureAttempt is an observation of a bounded, process-local native guard.
// Only Checkpoint proves publication; Ready never authorizes use of a snapshot.
type CaptureAttempt struct {
	ID         string           `json:"attempt_id"`
	SessionID  string           `json:"session_id"`
	State      string           `json:"state"`
	ExpiresAt  time.Time        `json:"expires_at"`
	Boundary   *CaptureBoundary `json:"boundary,omitempty"`
	Reference  *Reference       `json:"reference,omitempty"`
	Checkpoint *Checkpoint      `json:"checkpoint,omitempty"`
}

type captureAttempt struct {
	view      CaptureAttempt
	cancel    context.CancelFunc
	confirm   chan struct{}
	confirmed bool
}

// Captures retains only live guards and short-lived replies. Published facts
// remain in the checkpoint store. Process loss invalidates every pending guard.
type Captures struct {
	mu       sync.Mutex
	attempts map[string]*captureAttempt
	closed   bool
}

func (c *Captures) Start(sessionID, sandboxID string, service Service) (CaptureAttempt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return CaptureAttempt{}, ErrCaptureConflict
	}
	now := time.Now()
	for id, attempt := range c.attempts {
		if captureTerminal(attempt.view.State) && now.After(attempt.view.ExpiresAt.Add(captureRetention)) {
			delete(c.attempts, id)
		}
		if attempt.view.SessionID == sessionID && !captureTerminal(attempt.view.State) {
			return CaptureAttempt{}, ErrCaptureConflict
		}
	}
	if len(c.attempts) >= maxCaptures {
		return CaptureAttempt{}, ErrCaptureConflict
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return CaptureAttempt{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), CaptureLifetime)
	attempt := &captureAttempt{view: CaptureAttempt{ID: hex.EncodeToString(identity[:]), SessionID: sessionID, State: "capturing", ExpiresAt: now.Add(CaptureLifetime)}, cancel: cancel, confirm: make(chan struct{})}
	if c.attempts == nil {
		c.attempts = make(map[string]*captureAttempt)
	}
	c.attempts[attempt.view.ID] = attempt
	go c.run(ctx, attempt, sandboxID, service)
	return attempt.view, nil
}

func (c *Captures) run(ctx context.Context, attempt *captureAttempt, sandboxID string, service Service) {
	defer attempt.cancel()
	checkpoint, err := service.CaptureWithPin(ctx, sandboxID, func(pinCtx context.Context, boundary CaptureBoundary, reference Reference) error {
		c.mu.Lock()
		if pinCtx.Err() != nil {
			c.mu.Unlock()
			return context.Canceled
		}
		attempt.view.State = "ready"
		attempt.view.Boundary = &boundary
		attempt.view.Reference = &reference
		deadline := time.Now().Add(CapturePinLifetime)
		if nativeDeadline, ok := pinCtx.Deadline(); ok && nativeDeadline.Before(deadline) {
			deadline = nativeDeadline
		}
		attempt.view.ExpiresAt = deadline
		c.mu.Unlock()
		timer := time.NewTimer(CapturePinLifetime)
		defer timer.Stop()
		select {
		case <-pinCtx.Done():
			return pinCtx.Err()
		case <-timer.C:
			return context.DeadlineExceeded
		case <-attempt.confirm:
			return nil
		}
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		attempt.view.State = "failed"
		if errors.Is(ctx.Err(), context.Canceled) {
			attempt.view.State = "cancelled"
		}
		return
	}
	attempt.view.State = "published"
	attempt.view.Checkpoint = &checkpoint
}

func captureTerminal(state string) bool {
	return state == "published" || state == "failed" || state == "cancelled"
}

func (c *Captures) Observe(id, action string) (CaptureAttempt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	attempt := c.attempts[id]
	if attempt == nil || time.Now().After(attempt.view.ExpiresAt.Add(captureRetention)) {
		return CaptureAttempt{}, ErrCaptureNotFound
	}
	switch action {
	case "status":
	case "commit":
		if attempt.confirmed {
			return attempt.view, nil
		}
		if attempt.view.State != "ready" || time.Now().After(attempt.view.ExpiresAt) {
			return CaptureAttempt{}, ErrCaptureConflict
		}
		attempt.confirmed = true
		attempt.view.State = "publishing"
		close(attempt.confirm)
	case "cancel":
		if !captureTerminal(attempt.view.State) {
			attempt.cancel()
		}
	default:
		return CaptureAttempt{}, ErrCaptureConflict
	}
	return attempt.view, nil
}

func (c *Captures) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, attempt := range c.attempts {
		attempt.cancel()
	}
}
