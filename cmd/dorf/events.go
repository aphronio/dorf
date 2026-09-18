package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlclient"
	"github.com/aphronio/dorf/internal/core"
)

func remoteEventSend(ctx context.Context, client *controlclient.Client, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("session event", flag.ContinueOnError)
	set.SetOutput(stderr)
	filename := set.String("input-file", "", "file containing native input")
	clientID := set.String("client-id", "", "native correlation ID (not a replay key)")
	refresh := set.Bool("refresh-skills", false, "refresh native skills")
	var files attachmentFlags
	set.Var(&files, "attach", "local attachment (repeatable)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 1 {
		return fmt.Errorf("session event requires one Session ID")
	}
	event, err := readEventInput(*filename, "session event", files)
	if err != nil {
		return err
	}
	event.ClientID, _, err = operationKey("input", *clientID, rand.Reader)
	if err != nil {
		return err
	}
	event.RefreshSkills = *refresh
	ack, err := client.SubmitEvent(ctx, set.Arg(0), event)
	if err != nil {
		return err
	}
	return writeJSON(stdout, ack)
}
func remoteNativeReadOrCancel(ctx context.Context, client *controlclient.Client, args []string, stdout io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("session %s requires one Session ID", args[0])
	}
	var result any
	var err error
	switch args[0] {
	case "turns":
		result, err = client.Turns(ctx, args[1])
	case "history":
		result, err = client.History(ctx, args[1])
	case "cancel":
		result, err = client.SubmitEvent(ctx, args[1], core.NativeEvent{Type: core.InputCancel})
	}
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}
func waitNativeReady(ctx context.Context, client *controlclient.Client, id string) (controlapi.Session, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		session, err := client.Session(ctx, id)
		if err != nil {
			return session, err
		}
		if !session.Admission.Open || session.Attention != nil {
			return session, fmt.Errorf("Session is unavailable for native input")
		}
		if session.Execution.State == "idle" || session.Execution.State == "working" {
			return session, nil
		}
		select {
		case <-ctx.Done():
			return session, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
