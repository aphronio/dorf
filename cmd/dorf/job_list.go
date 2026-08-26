package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlclient"
)

const maxJobListCursorBytes = 1024

type jobListCursor struct {
	Version    int    `json:"v"`
	AdmittedAt string `json:"admitted_at"`
	JobID      string `json:"id"`
}

func (a controlAPIJobs) List(ctx context.Context, limit int, cursor string) (controlapi.JobList, error) {
	if limit < 1 || limit > 100 {
		return controlapi.JobList{}, controlapi.ErrInvalidInput
	}
	var cursorAt time.Time
	var cursorID string
	if cursor != "" {
		var err error
		cursorAt, cursorID, err = decodeJobListCursor(cursor)
		if err != nil {
			return controlapi.JobList{}, err
		}
	}
	rows, err := a.store.ListSupportedJobs(ctx, limit+1, cursorAt, cursorID)
	if err != nil {
		return controlapi.JobList{}, err
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		encoded, err := encodeJobListCursor(rows[len(rows)-1].AdmittedAt, rows[len(rows)-1].ID)
		if err != nil {
			return controlapi.JobList{}, err
		}
		next = &encoded
	}
	jobs := make([]controlapi.JobSummary, 0, len(rows))
	for _, row := range rows {
		kind, ok := classifyControlJob(row.Workflow, row.WorkflowRevision)
		if !ok {
			return controlapi.JobList{}, fmt.Errorf("unsupported retained Job workflow %q revision %q", row.Workflow, row.WorkflowRevision)
		}
		jobs = append(jobs, controlapi.JobSummary{ID: row.ID, Kind: string(kind), AdmittedAt: row.AdmittedAt.UTC()})
	}
	return controlapi.JobList{Jobs: jobs, NextCursor: next}, nil
}

func encodeJobListCursor(admittedAt time.Time, jobID string) (string, error) {
	if admittedAt.IsZero() || !validCursorJobID(jobID) {
		return "", controlapi.ErrInvalidCursor
	}
	payload, err := json.Marshal(jobListCursor{
		Version: 1, AdmittedAt: admittedAt.UTC().Format(time.RFC3339Nano), JobID: jobID,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeJobListCursor(encoded string) (time.Time, string, error) {
	invalid := func() (time.Time, string, error) { return time.Time{}, "", controlapi.ErrInvalidCursor }
	if encoded == "" || len(encoded) > maxJobListCursorBytes {
		return invalid()
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(payload) == 0 || len(payload) > maxJobListCursorBytes {
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cursor jobListCursor
	if err := decoder.Decode(&cursor); err != nil {
		return invalid()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalid()
	}
	admittedAt, err := time.Parse(time.RFC3339Nano, cursor.AdmittedAt)
	if err != nil || cursor.Version != 1 || !validCursorJobID(cursor.JobID) ||
		admittedAt.UTC().Format(time.RFC3339Nano) != cursor.AdmittedAt {
		return invalid()
	}
	canonical, err := encodeJobListCursor(admittedAt, cursor.JobID)
	if err != nil || canonical != encoded {
		return invalid()
	}
	return admittedAt, cursor.JobID, nil
}

func validCursorJobID(value string) bool {
	return value != "" && len(value) <= 255 && value == strings.TrimSpace(value) && !strings.ContainsRune(value, 0)
}

func remoteJobList(ctx context.Context, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("job list", flag.ContinueOnError)
	set.SetOutput(stderr)
	limit := set.Int("limit", 50, "maximum Jobs to return (1-100)")
	cursor := set.String("cursor", "", "opaque continuation cursor")
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("job list does not accept positional arguments")
	}
	if *limit < 1 || *limit > 100 {
		return fmt.Errorf("job list limit must be between 1 and 100")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	list, err := client.ListJobs(ctx, *limit, *cursor)
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, list)
	}
	if len(list.Jobs) == 0 {
		fmt.Fprintln(stdout, "No Jobs found")
	} else {
		fmt.Fprintln(stdout, "Jobs")
		for _, job := range list.Jobs {
			fmt.Fprintf(stdout, "  %s  %s  %s\n", job.ID, job.Kind, job.AdmittedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	if list.NextCursor != nil {
		fmt.Fprintf(stdout, "Next: dorf job list --limit %d --cursor %s\n", *limit, *list.NextCursor)
	}
	return nil
}
