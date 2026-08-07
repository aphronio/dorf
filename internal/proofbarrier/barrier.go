// Package proofbarrier contains deliberately awkward, bounded SIGKILL proof
// hooks. It is not a production fault-injection framework.
package proofbarrier

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const enablePhrase = "issue-41-external-sigkill-only"

type Barrier struct {
	Point    string
	Sequence int64
	Dir      string
	Wait     time.Duration
	Lease    time.Duration
}

func FromEnv() (spine.FaultBarrier, error) {
	point := strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER"))
	if point == "" {
		return nil, nil
	}
	if os.Getenv("DORF_PROOF_FAULT_BARRIER_ENABLE") != enablePhrase {
		return nil, fmt.Errorf("DORF_PROOF_FAULT_BARRIER requires the exact proof-only enable phrase %q", enablePhrase)
	}
	if point != spine.BarrierBeforeSubmit && point != spine.BarrierAfterSubmitBeforeBind && point != spine.BarrierNativeActive {
		return nil, fmt.Errorf("unsupported proof fault barrier %q", point)
	}
	sequence, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER_SEQUENCE")), 10, 64)
	if err != nil || sequence < 1 {
		return nil, fmt.Errorf("DORF_PROOF_FAULT_BARRIER_SEQUENCE must be a positive integer")
	}
	dir := strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER_DIR"))
	if dir == "" {
		return nil, fmt.Errorf("DORF_PROOF_FAULT_BARRIER_DIR is required in proof mode")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return Barrier{Point: point, Sequence: sequence, Dir: dir, Wait: 8 * time.Second, Lease: 10 * time.Second}, nil
}

func (b Barrier) Reach(ctx context.Context, point string, delivery spine.Delivery) error {
	if point != b.Point || delivery.Message.Sequence != b.Sequence {
		return nil
	}
	if b.Wait <= 0 || b.Wait > 30*time.Second || b.Lease <= b.Wait || b.Lease > time.Minute {
		return fmt.Errorf("unsafe proof barrier timing")
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	base := fmt.Sprintf("%s-seq-%d-%s", delivery.Message.JobID, delivery.Message.Sequence, point)
	ready := filepath.Join(b.Dir, base+".ready")
	release := filepath.Join(b.Dir, base+".release")
	if _, err := os.Stat(ready); err == nil {
		return fmt.Errorf("stale proof barrier marker exists: %s", ready)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := absurd.Heartbeat(ctx, b.Lease); err != nil {
		return fmt.Errorf("shorten proof claim lease: %w", err)
	}
	payload := fmt.Sprintf("job=%s\nsequence=%d\nmessage=%s\nagent_run=%s\npoint=%s\n", delivery.Message.JobID, delivery.Message.Sequence, delivery.Message.ID, delivery.AgentRun.ID, point)
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
	return fmt.Errorf("proof barrier %s timed out before its shortened claim lease; SIGKILL was not observed", point)
}
