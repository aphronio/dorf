package controlreader

import (
	"context"
	"net/http"

	"github.com/aphronio/dorf/internal/persistence"
)

const (
	CheckpointBoundaryPath = "/v1/checkpoints/boundary"
	CaptureStartPath       = "/v1/checkpoints/capture"
	CaptureObservePath     = "/v1/checkpoints/capture-observation"
	BranchStartPath        = "/v1/checkpoints/branch"
	BranchObservePath      = "/v1/checkpoints/branch-observation"
)

type captureRequest struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}
type branchObservationRequest struct {
	ID      string `json:"id"`
	Release bool   `json:"release"`
}

func checkpointEndpoint[I, O any](operations persistence.Operations, call func(context.Context, I) (O, error)) http.HandlerFunc {
	return jsonEndpoint(MaxRequestBytes, func(ctx context.Context, input I) (O, error) {
		if operations == nil {
			var zero O
			return zero, ErrUnavailable
		}
		return call(ctx, input)
	})
}

func checkpointRoutes(operations persistence.Operations) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		CheckpointBoundaryPath: checkpointEndpoint(operations, func(ctx context.Context, input observationRequest) (persistence.BoundaryObservation, error) {
			return operations.CheckpointBoundary(ctx, input.SessionID)
		}),
		CaptureStartPath: checkpointEndpoint(operations, func(ctx context.Context, input observationRequest) (persistence.CaptureAttempt, error) {
			return operations.StartCapture(ctx, input.SessionID)
		}),
		CaptureObservePath: checkpointEndpoint(operations, func(ctx context.Context, input captureRequest) (persistence.CaptureAttempt, error) {
			return operations.ObserveCapture(ctx, input.ID, input.Action)
		}),
		BranchStartPath: checkpointEndpoint(operations, func(ctx context.Context, input persistence.BranchRequest) (persistence.BranchReceipt, error) {
			return operations.BranchCheckpoint(ctx, input)
		}),
		BranchObservePath: checkpointEndpoint(operations, func(ctx context.Context, input branchObservationRequest) (persistence.BranchReceipt, error) {
			return operations.ObserveBranch(ctx, input.ID, input.Release)
		}),
	}
}

func checkpointResponse[T any](ctx context.Context, c Client, path string, input any) (T, error) {
	var result T
	response, err := c.request(ctx, path, input)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, decodeProblem(response)
	}
	err = decodeJSONResponse(response, &result, MaxRequestBytes, "checkpoint")
	return result, err
}
func (c Client) CheckpointBoundary(ctx context.Context, id string) (persistence.BoundaryObservation, error) {
	return checkpointResponse[persistence.BoundaryObservation](ctx, c, CheckpointBoundaryPath, observationRequest{SessionID: id})
}
func (c Client) StartCapture(ctx context.Context, id string) (persistence.CaptureAttempt, error) {
	return checkpointResponse[persistence.CaptureAttempt](ctx, c, CaptureStartPath, observationRequest{SessionID: id})
}
func (c Client) ObserveCapture(ctx context.Context, id, action string) (persistence.CaptureAttempt, error) {
	return checkpointResponse[persistence.CaptureAttempt](ctx, c, CaptureObservePath, captureRequest{ID: id, Action: action})
}
func (c Client) BranchCheckpoint(ctx context.Context, request persistence.BranchRequest) (persistence.BranchReceipt, error) {
	return checkpointResponse[persistence.BranchReceipt](ctx, c, BranchStartPath, request)
}
func (c Client) ObserveBranch(ctx context.Context, id string, release bool) (persistence.BranchReceipt, error) {
	return checkpointResponse[persistence.BranchReceipt](ctx, c, BranchObservePath, branchObservationRequest{ID: id, Release: release})
}
func (c Client) Close() {}

func (s Service) CheckpointBoundary(ctx context.Context, id string) (persistence.BoundaryObservation, error) {
	if s.Checkpoints == nil {
		return persistence.BoundaryObservation{}, ErrUnavailable
	}
	return s.Checkpoints.CheckpointBoundary(ctx, id)
}
func (s Service) StartCapture(ctx context.Context, id string) (persistence.CaptureAttempt, error) {
	if s.Checkpoints == nil {
		return persistence.CaptureAttempt{}, ErrUnavailable
	}
	return s.Checkpoints.StartCapture(ctx, id)
}
func (s Service) ObserveCapture(ctx context.Context, id, action string) (persistence.CaptureAttempt, error) {
	if s.Checkpoints == nil {
		return persistence.CaptureAttempt{}, ErrUnavailable
	}
	return s.Checkpoints.ObserveCapture(ctx, id, action)
}
func (s Service) BranchCheckpoint(ctx context.Context, request persistence.BranchRequest) (persistence.BranchReceipt, error) {
	if s.Checkpoints == nil {
		return persistence.BranchReceipt{}, ErrUnavailable
	}
	return s.Checkpoints.BranchCheckpoint(ctx, request)
}
func (s Service) ObserveBranch(ctx context.Context, id string, release bool) (persistence.BranchReceipt, error) {
	if s.Checkpoints == nil {
		return persistence.BranchReceipt{}, ErrUnavailable
	}
	return s.Checkpoints.ObserveBranch(ctx, id, release)
}
func (s Service) Close() {
	if s.Checkpoints != nil {
		s.Checkpoints.Close()
	}
}
