package controlreader

import (
	"context"
	"errors"
	"net/http"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

const TimelinePath = "/v1/jobs/timeline"

var ErrJobNotFound = errors.New("control reader Job not found")

type timelineStore interface {
	Sandboxes(context.Context, string) ([]core.Sandbox, error)
	Deliveries(context.Context, string) ([]core.Delivery, error)
}

type timelineRequest struct {
	JobID  string `json:"job_id"`
	TurnID string `json:"turn_id,omitempty"`
}

func (s Service) ReadTimeline(ctx context.Context, jobID, turnID string) (core.HarnessTimeline, error) {
	if !validIdentity(jobID) {
		return core.HarnessTimeline{}, ErrJobNotFound
	}
	if turnID != "" && !validIdentity(turnID) {
		return core.HarnessTimeline{}, ErrInvalidRequest
	}
	store, ok := s.Store.(timelineStore)
	if !ok || s.Runtimes == nil {
		return core.HarnessTimeline{}, core.ErrTimelineUnavailable
	}
	var result core.HarnessTimeline
	var idleRuntime core.Execution
	defer func() { core.ReconcileIdle(ctx, idleRuntime, jobID) }()
	err := s.Store.WithJobFence(ctx, jobID, func() error {
		job, err := s.Store.Job(ctx, jobID)
		if errors.Is(err, postgres.ErrNotFound) {
			return ErrJobNotFound
		}
		if err != nil {
			return err
		}
		if job.ID != jobID || job.CleanupState != core.CleanupPending {
			return core.ErrTimelineUnavailable
		}
		owned, harness, threadID, err := timelineBinding(ctx, store, jobID)
		if err != nil {
			return err
		}
		runtime, job, err := s.sandboxAuthority(ctx, owned)
		if err != nil || runtime.Timeline == nil {
			return core.ErrTimelineUnavailable
		}
		idleRuntime = runtime.Execution
		err = core.WithSandboxActivity(ctx, s.Store, job.ID, func() error {
			result, err = runtime.Timeline.ReadTimeline(ctx, job, owned, threadID, turnID)
			return err
		})
		if err != nil {
			return errors.Join(core.ErrTimelineUnavailable, err)
		}
		return validateTimeline(result, harness, threadID, turnID)
	})
	if err != nil {
		return core.HarnessTimeline{}, err
	}
	return result, nil
}

func timelineBinding(ctx context.Context, store timelineStore, jobID string) (core.Sandbox, string, string, error) {
	sandboxes, err := store.Sandboxes(ctx, jobID)
	if err != nil {
		return core.Sandbox{}, "", "", err
	}
	owned, err := defaultTimelineSandbox(sandboxes, jobID)
	if err != nil {
		return core.Sandbox{}, "", "", err
	}
	deliveries, err := store.Deliveries(ctx, jobID)
	if err != nil {
		return core.Sandbox{}, "", "", err
	}
	var harness, threadID string
	for _, delivery := range deliveries {
		run := delivery.AgentRun
		if run.SandboxID != owned.ID || run.ThreadID == "" {
			continue
		}
		if run.JobID != jobID || delivery.Message.JobID != jobID || !validIdentity(run.ThreadID) || !validIdentity(run.Harness) {
			return core.Sandbox{}, "", "", core.ErrTimelineUnavailable
		}
		if threadID != "" && (threadID != run.ThreadID || harness != run.Harness) {
			return core.Sandbox{}, "", "", core.ErrTimelineUnavailable
		}
		harness, threadID = run.Harness, run.ThreadID
	}
	if threadID == "" {
		return core.Sandbox{}, "", "", core.ErrTimelineUnavailable
	}
	return owned, harness, threadID, nil
}

func (c Client) ReadTimeline(ctx context.Context, jobID, turnID string) (core.HarnessTimeline, error) {
	response, err := c.request(ctx, TimelinePath, timelineRequest{JobID: jobID, TurnID: turnID})
	if err != nil {
		return core.HarnessTimeline{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return core.HarnessTimeline{}, decodeProblem(response)
	}
	var result core.HarnessTimeline
	if err := decodeJSONResponse(response, &result, MaxObservationBytes, "timeline"); err != nil {
		return core.HarnessTimeline{}, err
	}
	return result, nil
}

func validateTimeline(result core.HarnessTimeline, harness, threadID, turnID string) error {
	if result.Harness != harness || result.ThreadID != threadID || result.TurnID == "" || turnID != "" && result.TurnID != turnID || len(result.Items) == 0 {
		return core.ErrTimelineUnavailable
	}
	if result.Status != "inProgress" && !terminalMessageOutcome(result.Status) {
		return core.ErrTimelineUnavailable
	}
	return nil
}

func defaultTimelineSandbox(sandboxes []core.Sandbox, jobID string) (core.Sandbox, error) {
	var owned core.Sandbox
	for _, candidate := range sandboxes {
		if candidate.Name != core.DefaultSandbox {
			continue
		}
		if owned.ID != "" || candidate.JobID != jobID || !validIdentity(candidate.ID) || !validIdentity(candidate.OwnershipNonce) {
			return core.Sandbox{}, core.ErrTimelineUnavailable
		}
		owned = candidate
	}
	if owned.ID == "" {
		return core.Sandbox{}, core.ErrTimelineUnavailable
	}
	return owned, nil
}
