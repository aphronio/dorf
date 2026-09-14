package e2b

import (
	"context"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

func (a Adapter) ReadFiles(ctx context.Context, owner provider.Ownership, names []string, maxBytes int) (map[string][]byte, error) {
	return provider.ReadFilesViaExec(ctx, owner, a.Workspace(), names, maxBytes, a.Exec)
}
