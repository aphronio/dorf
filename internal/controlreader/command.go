package controlreader

import (
	"context"
	"net/http"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

const CommandPath = "/v1/sandboxes/exec"

type commandRequest struct {
	SandboxID string `json:"sandbox_id"`
	provider.Command
}

func (s Service) Exec(ctx context.Context, sandboxID string, command provider.Command) (provider.CommandResult, error) {
	if err := command.Validate(); err != nil {
		return provider.CommandResult{}, ErrInvalidRequest
	}
	var result provider.CommandResult
	err := s.withSandbox(ctx, sandboxID, func(runtime core.SandboxRuntime, job core.Job, owned core.Sandbox) error {
		executor := runtime.Commands
		if executor == nil {
			return ErrUnavailable
		}
		var err error
		result, err = executor.ExecSandbox(ctx, job, owned, command)
		return err
	})
	return result, err
}

func commandEndpoint(service Service) http.HandlerFunc {
	return jsonEndpoint(provider.MaxCommandResponseBytes, func(ctx context.Context, request commandRequest) (provider.CommandResult, error) {
		return service.Exec(ctx, request.SandboxID, request.Command)
	})
}

func (c Client) Exec(ctx context.Context, sandboxID string, command provider.Command) (provider.CommandResult, error) {
	httpClient := *c.http
	httpClient.Timeout = command.Timeout() + 5*time.Second
	c.http = &httpClient
	response, err := c.request(ctx, CommandPath, commandRequest{SandboxID: sandboxID, Command: command})
	if err != nil {
		return provider.CommandResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return provider.CommandResult{}, decodeProblem(response)
	}
	var result provider.CommandResult
	err = decodeJSONResponse(response, &result, provider.MaxCommandResponseBytes, "Sandbox command")
	return result, err
}
