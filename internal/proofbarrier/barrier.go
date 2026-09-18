// Package proofbarrier contains deliberately awkward, bounded SIGKILL proof
// hooks. It is not a production fault-injection framework.
package proofbarrier

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const proofEnablePhrase = "external-sigkill-proof-only"

type Barrier struct {
	Point     string
	SessionID string
	Dir       string
	Wait      time.Duration
	Lease     time.Duration
}

func FromEnv() (core.FaultBarrier, error) {
	point := strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER"))
	if point == "" {
		return nil, nil
	}
	if point != core.BarrierRouteRevoked && point != core.BarrierSandboxDeleted && point != core.BarrierSandboxCreated {
		return nil, fmt.Errorf("unsupported proof barrier %q", point)
	}
	if os.Getenv("DORF_PROOF_FAULT_BARRIER_ENABLE") != proofEnablePhrase {
		return nil, fmt.Errorf("proof-only enable phrase required")
	}
	sessionID := strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER_SESSION"))
	if sessionID == "" {
		return nil, fmt.Errorf("proof Session is required")
	}
	dir := strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER_DIR"))
	if dir == "" {
		return nil, fmt.Errorf("DORF_PROOF_FAULT_BARRIER_DIR is required in proof mode")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return Barrier{Point: point, SessionID: sessionID, Dir: dir, Wait: 8 * time.Second, Lease: 10 * time.Second}, nil
}

func (b Barrier) ReachOperation(ctx context.Context, point, sessionID, identity string) error {
	if point != b.Point || sessionID != b.SessionID {
		return nil
	}
	return b.reach(ctx, sessionID, identity, point, fmt.Sprintf("session=%s\nidentity=%s\npoint=%s\n", sessionID, identity, point), true)
}

func (b Barrier) reach(ctx context.Context, sessionID, identity, point, payload string, heartbeat bool) error {
	if b.Wait <= 0 || b.Wait > 30*time.Second || heartbeat && (b.Lease <= b.Wait || b.Lease > time.Minute) {
		return fmt.Errorf("unsafe proof barrier timing")
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	base := fmt.Sprintf("%s-%s-%s", sessionID, identity, point)
	ready := filepath.Join(b.Dir, base+".ready")
	release := filepath.Join(b.Dir, base+".release")
	if recovered, err := recoverReady(ready, payload); err != nil {
		return err
	} else if recovered {
		return nil
	}
	if heartbeat {
		if err := absurd.Heartbeat(ctx, b.Lease); err != nil {
			return fmt.Errorf("shorten proof claim lease: %w", err)
		}
	}
	if err := os.WriteFile(ready, []byte(payload), 0o600); err != nil {
		return err
	}
	deadline := time.Now().Add(b.Wait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(release); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if heartbeat {
		return fmt.Errorf("proof barrier %s timed out before its shortened claim lease; SIGKILL was not observed", point)
	}
	return fmt.Errorf("proof barrier %s timed out; SIGKILL was not observed", point)
}

func recoverReady(path, expected string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Size() != int64(len(expected)) {
		return false, fmt.Errorf("proof barrier marker conflicts with exact bounded payload: %s", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if len(contents) != len(expected) || string(contents) != expected {
		return false, fmt.Errorf("proof barrier marker conflicts with exact bounded payload: %s", path)
	}
	return true, nil
}
