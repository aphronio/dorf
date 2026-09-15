package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// BindSandboxResource records an attested locator without allowing a later
// observation to redirect the same resource identity. Callers hold the Job fence
// across provider attestation and this write.
func (s Store) BindSandboxResource(ctx context.Context, owned core.Sandbox, providerID string) error {
	if owned.JobID == "" || owned.ID == "" || owned.ResourceID == "" || strings.TrimSpace(providerID) == "" || providerID != strings.TrimSpace(providerID) {
		return fmt.Errorf("resource binding requires exact ownership and provider identity")
	}
	return expectOneRows(dbsql.New(s.DB).BindSandboxResource(ctx, dbsql.BindSandboxResourceParams{
		JobID: owned.JobID, SandboxID: owned.ID, ResourceID: owned.ResourceID, OwnershipNonce: owned.OwnershipNonce, ProviderID: providerID,
	}))
}

func (s Store) SandboxResources(ctx context.Context, jobID string) ([]core.SandboxResource, error) {
	rows, err := dbsql.New(s.DB).ListSandboxResources(ctx, jobID)
	if err != nil {
		return nil, err
	}
	resources := make([]core.SandboxResource, 0, len(rows))
	for _, row := range rows {
		resources = append(resources, core.SandboxResource{
			ID: row.ID, SandboxID: row.SandboxID, OwnershipNonce: row.OwnershipNonce, ProviderID: row.ProviderID,
			ReservedAt: row.ReservedAt, ObservedAt: timeValue(row.ObservedAt), DeletedAt: timeValue(row.DeletedAt),
		})
	}
	return resources, nil
}

func (s Store) SandboxResource(ctx context.Context, jobID, sandboxID, resourceID string) (core.Sandbox, error) {
	row, err := dbsql.New(s.DB).GetSandboxResource(ctx, dbsql.GetSandboxResourceParams{
		JobID: jobID, SandboxID: sandboxID, ResourceID: resourceID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return core.Sandbox{}, ErrNotFound
	}
	if err != nil {
		return core.Sandbox{}, err
	}
	return core.Sandbox{ID: row.ID, JobID: row.JobID, Name: row.Name,
		ResourceID: row.ResourceID, OwnershipNonce: row.OwnershipNonce, ProviderID: row.ProviderID}, nil
}

func (s Store) RecordSandboxResourceDeleted(ctx context.Context, owned core.Sandbox) error {
	return expectOneRows(dbsql.New(s.DB).RecordSandboxResourceDeleted(ctx, dbsql.RecordSandboxResourceDeletedParams{ResourceID: owned.ResourceID, SandboxID: owned.ID}))
}
