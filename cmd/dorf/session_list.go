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

const maxSessionListCursorBytes = 1024

type sessionListCursor struct {
	Version    int    `json:"v"`
	AdmittedAt string `json:"admitted_at"`
	SessionID  string `json:"id"`
}

func (a controlAPISessions) List(ctx context.Context, limit int, cursor string) (controlapi.SessionList, error) {
	if limit < 1 || limit > 100 {
		return controlapi.SessionList{}, controlapi.ErrInvalidInput
	}
	var cursorAt time.Time
	var cursorID string
	if cursor != "" {
		var err error
		cursorAt, cursorID, err = decodeSessionListCursor(cursor)
		if err != nil {
			return controlapi.SessionList{}, err
		}
	}
	rows, err := a.store.ListSessions(ctx, limit+1, cursorAt, cursorID)
	if err != nil {
		return controlapi.SessionList{}, err
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		encoded, err := encodeSessionListCursor(rows[len(rows)-1].AdmittedAt, rows[len(rows)-1].ID)
		if err != nil {
			return controlapi.SessionList{}, err
		}
		next = &encoded
	}
	sessions := make([]controlapi.SessionSummary, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, controlapi.SessionSummary{CreatedByClient: publicSessionCreator(row.CreatedByClientID, row.CreatedByClientName), ClientReference: row.ClientReference, ID: row.ID, AdmittedAt: row.AdmittedAt.UTC()})
	}
	return controlapi.SessionList{Sessions: sessions, NextCursor: next}, nil
}

func encodeSessionListCursor(admittedAt time.Time, sessionID string) (string, error) {
	if admittedAt.IsZero() || !validCursorSessionID(sessionID) {
		return "", controlapi.ErrInvalidCursor
	}
	payload, err := json.Marshal(sessionListCursor{
		Version: 1, AdmittedAt: admittedAt.UTC().Format(time.RFC3339Nano), SessionID: sessionID,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeSessionListCursor(encoded string) (time.Time, string, error) {
	invalid := func() (time.Time, string, error) { return time.Time{}, "", controlapi.ErrInvalidCursor }
	if encoded == "" || len(encoded) > maxSessionListCursorBytes {
		return invalid()
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(payload) == 0 || len(payload) > maxSessionListCursorBytes {
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cursor sessionListCursor
	if err := decoder.Decode(&cursor); err != nil {
		return invalid()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invalid()
	}
	admittedAt, err := time.Parse(time.RFC3339Nano, cursor.AdmittedAt)
	if err != nil || cursor.Version != 1 || !validCursorSessionID(cursor.SessionID) ||
		admittedAt.UTC().Format(time.RFC3339Nano) != cursor.AdmittedAt {
		return invalid()
	}
	canonical, err := encodeSessionListCursor(admittedAt, cursor.SessionID)
	if err != nil || canonical != encoded {
		return invalid()
	}
	return admittedAt, cursor.SessionID, nil
}

func validCursorSessionID(value string) bool {
	return value != "" && len(value) <= 255 && value == strings.TrimSpace(value) && !strings.ContainsRune(value, 0)
}

func remoteSessionList(ctx context.Context, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("session list", flag.ContinueOnError)
	set.SetOutput(stderr)
	limit := set.Int("limit", 50, "maximum Sessions to return (1-100)")
	cursor := set.String("cursor", "", "opaque continuation cursor")
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("session list does not accept positional arguments")
	}
	if *limit < 1 || *limit > 100 {
		return fmt.Errorf("session list limit must be between 1 and 100")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	list, err := client.ListSessions(ctx, *limit, *cursor)
	if err != nil {
		return err
	}
	if *output == "json" {
		return writeJSON(stdout, list)
	}
	if len(list.Sessions) == 0 {
		fmt.Fprintln(stdout, "No Sessions found")
	} else {
		fmt.Fprintln(stdout, "Sessions")
		for _, session := range list.Sessions {
			fmt.Fprintf(stdout, "  %s  %s\n", session.ID, session.AdmittedAt.UTC().Format(time.RFC3339Nano))
			renderSessionAttribution(stdout, session.CreatedByClient, session.ClientReference)
		}
	}
	if list.NextCursor != nil {
		fmt.Fprintf(stdout, "Next: dorf session list --limit %d --cursor %s\n", *limit, *list.NextCursor)
	}
	return nil
}
