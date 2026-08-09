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

const (
	messageEnablePhrase     = "issue-41-external-sigkill-only"
	workflowEnablePhrase    = "issue-37-external-sigkill-only"
	publicationEnablePhrase = "issue-43-external-sigkill-only"
	cleanupEnablePhrase     = "issue-39-external-sigkill-only"
)

type Barrier struct {
	Point    string
	Sequence int64
	JobID    string
	Dir      string
	Wait     time.Duration
	Lease    time.Duration
}

func FromEnv() (spine.FaultBarrier, error) {
	point := strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER"))
	if point == "" {
		return nil, nil
	}
	messagePoint := point == spine.BarrierBeforeSubmit || point == spine.BarrierAfterSubmitBeforeBind || point == spine.BarrierNativeActive
	workflowPoint := point == spine.BarrierSetupComplete || point == spine.BarrierCheckExited
	publicationPoint := point == spine.BarrierPushAccepted || point == spine.BarrierPullRequestAccepted || point == spine.BarrierPublicationBegin || point == spine.BarrierPublicationSpawn
	cleanupPoint := point == spine.BarrierReviewerRouteRevoked || point == spine.BarrierReviewerSandboxDeleted || point == spine.BarrierMainRouteRevoked || point == spine.BarrierMainSandboxDeleted
	if !messagePoint && !workflowPoint && !publicationPoint && !cleanupPoint {
		return nil, fmt.Errorf("unsupported proof fault barrier %q", point)
	}
	phrase := workflowEnablePhrase
	if publicationPoint {
		phrase = publicationEnablePhrase
	}
	if cleanupPoint {
		phrase = cleanupEnablePhrase
	}
	if messagePoint {
		phrase = messageEnablePhrase
	}
	if os.Getenv("DORF_PROOF_FAULT_BARRIER_ENABLE") != phrase {
		return nil, fmt.Errorf("DORF_PROOF_FAULT_BARRIER requires the exact proof-only enable phrase %q", phrase)
	}
	var sequence int64
	jobID := strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER_JOB"))
	if messagePoint {
		var err error
		sequence, err = strconv.ParseInt(strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER_SEQUENCE")), 10, 64)
		if err != nil || sequence < 1 {
			return nil, fmt.Errorf("DORF_PROOF_FAULT_BARRIER_SEQUENCE must be a positive integer")
		}
	} else if jobID == "" {
		return nil, fmt.Errorf("DORF_PROOF_FAULT_BARRIER_JOB is required for repository proof boundaries")
	}
	dir := strings.TrimSpace(os.Getenv("DORF_PROOF_FAULT_BARRIER_DIR"))
	if dir == "" {
		return nil, fmt.Errorf("DORF_PROOF_FAULT_BARRIER_DIR is required in proof mode")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return Barrier{Point: point, Sequence: sequence, JobID: jobID, Dir: dir, Wait: 8 * time.Second, Lease: 10 * time.Second}, nil
}

func (b Barrier) ReachWorkflow(ctx context.Context, point, jobID, identity string) error {
	if point != b.Point || jobID != b.JobID {
		return nil
	}
	scheduling := point == spine.BarrierPublicationBegin || point == spine.BarrierPublicationSpawn
	return b.reach(ctx, jobID, identity, point, fmt.Sprintf("job=%s\nidentity=%s\npoint=%s\n", jobID, identity, point), !scheduling)
}

func (b Barrier) reach(ctx context.Context, jobID, identity, point, payload string, heartbeat bool) error {
	if b.Wait <= 0 || b.Wait > 30*time.Second || heartbeat && (b.Lease <= b.Wait || b.Lease > time.Minute) {
		return fmt.Errorf("unsafe proof barrier timing")
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	base := fmt.Sprintf("%s-%s-%s", jobID, identity, point)
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
	payload := fmt.Sprintf("job=%s\nsequence=%d\nmessage=%s\nagent_run=%s\npoint=%s\n", delivery.Message.JobID, delivery.Message.Sequence, delivery.Message.ID, delivery.AgentRun.ID, point)
	if recovered, err := recoverReady(ready, payload); err != nil {
		return err
	} else if recovered {
		return nil
	}
	if err := absurd.Heartbeat(ctx, b.Lease); err != nil {
		return fmt.Errorf("shorten proof claim lease: %w", err)
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
	return fmt.Errorf("proof barrier %s timed out before its shortened claim lease; SIGKILL was not observed", point)
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
