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
	CaptureLifetime  = 3 * time.Minute
	captureRetention = 5 * time.Minute
	maxCaptures      = 32
)

var (
	ErrCaptureNotFound = errors.New("capture attempt is unavailable")
	ErrCaptureConflict = errors.New("capture attempt cannot accept this operation")
)

// CaptureAttempt reports copying, background upload, and publication.
// Uploading means the native copy is stable; only Ready is restorable.
type CaptureAttempt struct {
	ID         string           `json:"id"`
	SessionID  string           `json:"session_id"`
	State      string           `json:"status"`
	ExpiresAt  time.Time        `json:"expires_at"`
	Boundary   *CaptureBoundary `json:"boundary,omitempty"`
	Reference  *Reference       `json:"reference,omitempty"`
	Checkpoint *Checkpoint      `json:"checkpoint,omitempty"`
}

type captureAttempt struct {
	view   CaptureAttempt
	cancel context.CancelFunc
}

// Captures retains bounded uploads and short-lived observations. Published facts
// remain in the checkpoint store. Process loss abandons unfinished saves.
type Captures struct {
	mu       sync.Mutex
	attempts map[string]*captureAttempt
	closed   bool
}

func (c *Captures) Start(sessionID, sandboxID string, service Service, keys ...string) (CaptureAttempt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return CaptureAttempt{}, ErrCaptureConflict
	}
	if len(keys) > 0 && keys[0] != "" {
		if old := c.attempts[keys[0]]; old != nil {
			return old.view, nil
		}
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
	id := hex.EncodeToString(identity[:])
	if len(keys) > 0 && keys[0] != "" {
		id = keys[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), CaptureLifetime)
	attempt := &captureAttempt{view: CaptureAttempt{ID: id, SessionID: sessionID, State: "copying", ExpiresAt: now.Add(CaptureLifetime)}, cancel: cancel}
	if c.attempts == nil {
		c.attempts = make(map[string]*captureAttempt)
	}
	c.attempts[attempt.view.ID] = attempt
	go c.run(ctx, attempt, sandboxID, service)
	return attempt.view, nil
}

func (c *Captures) run(ctx context.Context, attempt *captureAttempt, sandboxID string, service Service) {
	defer attempt.cancel()
	checkpoint, err := service.CaptureCopy(ctx, sandboxID, attempt.view.ID, func(boundary CaptureBoundary) {
		c.mu.Lock()
		defer c.mu.Unlock()
		attempt.view.State = "uploading"
		attempt.view.Boundary = &boundary
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
	attempt.view.State = "ready"
	attempt.view.Checkpoint = &checkpoint
}

func captureTerminal(state string) bool {
	return state == "ready" || state == "failed" || state == "cancelled"
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
