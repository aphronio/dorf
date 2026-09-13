package controlreader

import (
	"context"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"net/http"
)

const StatusPath = "/v1/sandboxes/status"

type statusRequest struct {
	SandboxID string `json:"sandbox_id"`
}

func (s Service) ReadSandboxStatus(ctx context.Context, sandboxID string) (provider.Status, error) {
	var result provider.Status
	// Observation is fenced against cleanup but must never reconcile idle or wake the VM.
	err := s.accessSandbox(ctx, sandboxID, false, func(runtime core.SandboxRuntime, job core.Job, owned core.Sandbox) error {
		if runtime.Status == nil {
			return ErrUnavailable
		}
		var err error
		result, err = runtime.Status.ReadSandboxStatus(ctx, job, owned)
		return err
	})
	return result, err
}

func statusEndpoint(service Service) http.HandlerFunc {
	return jsonEndpoint(4096, func(ctx context.Context, request statusRequest) (provider.Status, error) {
		return service.ReadSandboxStatus(ctx, request.SandboxID)
	})
}

func (c Client) ReadSandboxStatus(ctx context.Context, sandboxID string) (provider.Status, error) {
	response, err := c.request(ctx, StatusPath, statusRequest{SandboxID: sandboxID})
	if err != nil {
		return provider.Status{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return provider.Status{}, decodeProblem(response)
	}
	var result provider.Status
	err = decodeJSONResponse(response, &result, 4096, "Sandbox status")
	return result, err
}
