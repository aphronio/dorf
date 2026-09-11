package controlreader

import (
	"context"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"net/http"
)

type fileWriteRequest struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
	Contents  []byte `json:"contents"`
	IfAbsent  bool   `json:"if_absent"`
}

func (s Service) WriteFile(ctx context.Context, sandboxID, name string, contents []byte, ifAbsent bool) error {
	if err := provider.ValidateFilePath(name); err != nil {
		return ErrInvalidFilePath
	}
	if len(contents) > provider.MaxFileWriteBytes {
		return ErrInvalidRequest
	}
	return s.withSandbox(ctx, sandboxID, func(runtime core.SandboxRuntime, job core.Job, owned core.Sandbox) error {
		writer, ok := runtime.Files.(core.SandboxFileWriter)
		if !ok {
			return ErrUnavailable
		}
		return writer.WriteSandboxFile(ctx, job, owned, name, contents, ifAbsent)
	})
}

func fileWriteEndpoint(service Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input fileWriteRequest
		if !decodeRequest(w, r, &input) {
			return
		}
		if err := service.WriteFile(r.Context(), input.SandboxID, input.Path, input.Contents, input.IfAbsent); err != nil {
			writeServiceError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (c Client) WriteFile(ctx context.Context, sandboxID, name string, contents []byte, ifAbsent bool) error {
	response, err := c.request(ctx, FileWritePath, fileWriteRequest{SandboxID: sandboxID, Path: name, Contents: contents, IfAbsent: ifAbsent})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return decodeProblem(response)
	}
	return nil
}
