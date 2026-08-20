package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

var ErrArtifactNotFound = errors.New("Dorf Artifact not found")

func (s Store) Artifact(ctx context.Context, artifactID string) (core.Artifact, error) {
	row, err := dbsql.New(s.DB).GetArtifact(ctx, artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return core.Artifact{}, err
	}
	return artifactFromRow(row), nil
}

func (s Store) Artifacts(ctx context.Context, jobID string) ([]core.Artifact, error) {
	rows, err := dbsql.New(s.DB).ListArtifacts(ctx, jobID)
	if err != nil {
		return nil, err
	}
	records := make([]core.Artifact, 0, len(rows))
	for _, row := range rows {
		records = append(records, artifactFromRow(row))
	}
	return records, nil
}

func insertArtifact(ctx context.Context, tx *sql.Tx, artifact core.Artifact) error {
	queries := dbsql.New(tx)
	if err := queries.InsertArtifact(ctx, dbsql.InsertArtifactParams{
		ID: artifact.ID, JobID: artifact.JobID, Name: artifact.Name,
		Digest: artifact.Digest, ByteSize: artifact.ByteSize, MediaType: artifact.MediaType,
		Producer: artifact.Producer, AgentRunID: artifact.AgentRunID, CreatedAt: artifact.CreatedAt,
	}); err != nil {
		return err
	}
	stored, err := queries.GetArtifact(ctx, artifact.ID)
	if err != nil {
		return err
	}
	storedArtifact := artifactFromRow(stored)
	if storedArtifact.ID != artifact.ID || storedArtifact.JobID != artifact.JobID || storedArtifact.Name != artifact.Name ||
		storedArtifact.Digest != artifact.Digest || storedArtifact.ByteSize != artifact.ByteSize || storedArtifact.MediaType != artifact.MediaType ||
		storedArtifact.Producer != artifact.Producer || storedArtifact.AgentRunID != artifact.AgentRunID || !storedArtifact.CreatedAt.Equal(artifact.CreatedAt) {
		return fmt.Errorf("Artifact identity %s conflicts with immutable retained metadata or content", artifact.ID)
	}
	return nil
}

func artifactFromRow(row dbsql.DorfArtifact) core.Artifact {
	return core.Artifact{
		ID: row.ID, JobID: row.JobID, Name: row.Name, Digest: row.Digest,
		ByteSize: row.ByteSize, MediaType: row.MediaType, Producer: row.Producer,
		AgentRunID: row.AgentRunID, CreatedAt: row.CreatedAt.UTC(),
	}
}
