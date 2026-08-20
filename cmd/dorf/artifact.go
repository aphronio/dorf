package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
)

type artifactListStore interface {
	Job(context.Context, string) (core.Job, error)
	Artifacts(context.Context, string) ([]core.Artifact, error)
}

type artifactGetStore interface {
	Artifact(context.Context, string) (core.Artifact, error)
}

type artifactCommandStore interface {
	artifactListStore
	artifactGetStore
}

func artifactCommand(ctx context.Context, store artifactCommandStore, records blob.Store, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("artifact requires: list JOB_ID or get ARTIFACT_ID")
	}
	switch args[0] {
	case "list":
		return artifactList(ctx, store, args[1:], stdout, stderr)
	case "get":
		return artifactGet(ctx, store, records, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("artifact requires: list JOB_ID or get ARTIFACT_ID")
	}
}

func artifactList(ctx context.Context, store artifactListStore, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("artifact list", flag.ContinueOnError)
	set.SetOutput(stderr)
	jsonOutput := set.Bool("json", false, "emit machine-readable JSON")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("artifact list requires one Job ID")
	}
	job, err := store.Job(ctx, set.Arg(0))
	if err != nil {
		return err
	}
	artifacts, err := store.Artifacts(ctx, job.ID)
	if err != nil {
		return err
	}
	if artifacts == nil {
		artifacts = []core.Artifact{}
	}
	if *jsonOutput {
		return writeJSON(stdout, map[string]any{"job_id": job.ID, "artifacts": artifacts})
	}
	if len(artifacts) == 0 {
		fmt.Fprintf(stdout, "Job %s has no Artifacts.\n", job.ID)
		return nil
	}
	fmt.Fprintf(stdout, "Artifacts for Job %s\n", job.ID)
	for _, artifact := range artifacts {
		fmt.Fprintf(stdout, "  %s  %s  %s  %d bytes\n", artifact.ID, artifact.Name, artifact.MediaType, artifact.ByteSize)
	}
	return nil
}

func artifactGet(ctx context.Context, store artifactGetStore, records blob.Store, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("artifact get", flag.ContinueOnError)
	set.SetOutput(stderr)
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("artifact get requires one Artifact ID")
	}
	artifact, err := store.Artifact(ctx, set.Arg(0))
	if err != nil {
		return err
	}
	contents, err := records.ReadVerified(artifact.Digest, artifact.ByteSize)
	if err != nil {
		return fmt.Errorf("Artifact %s: %w", artifact.ID, err)
	}
	_, err = stdout.Write(contents)
	return err
}
