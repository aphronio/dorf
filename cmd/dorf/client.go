package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/postgres"
)

type enrollmentReceipt struct {
	ID             string    `json:"id"`
	EnrollmentCode string    `json:"enrollment_code"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type clientList struct {
	Clients []controlauth.ClientRecord `json:"clients"`
}

func clientCommand(ctx context.Context, store postgres.Store, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("client requires: enroll, issue-key, list, show, or revoke")
	}
	if args[0] == "issue-key" {
		return issueClientKey(ctx, store, args[1:], stdout, stderr)
	}
	set := flag.NewFlagSet("client "+args[0], flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	auth := controlauth.Service{Store: store}
	switch args[0] {
	case "enroll":
		if set.NArg() != 0 {
			return fmt.Errorf("client enroll does not accept positional arguments")
		}
		enrollment, err := auth.CreateEnrollment(ctx)
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(stdout, enrollmentReceipt{ID: enrollment.ID, EnrollmentCode: enrollment.Token, ExpiresAt: enrollment.ExpiresAt})
		}
		fmt.Fprintf(stdout, "One-time Dorf enrollment (expires %s):\n%s\n", enrollment.ExpiresAt.Format(time.RFC3339), enrollment.Token)
		return nil
	case "list":
		if set.NArg() != 0 {
			return fmt.Errorf("client list does not accept positional arguments")
		}
		clients, err := auth.ListClients(ctx)
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(stdout, clientList{Clients: clients})
		}
		fmt.Fprintln(stdout, "Dorf Clients")
		for _, client := range clients {
			fmt.Fprintf(stdout, "  %s  %s  %s  expires %s\n", client.ID, client.Name, client.State, formatClientExpiry(client.ExpiresAt))
		}
		return nil
	case "show", "revoke":
		if set.NArg() != 1 {
			return fmt.Errorf("client %s requires one Client ID", args[0])
		}
		client, err := getOrRevokeClient(ctx, auth, args[0], set.Arg(0))
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(stdout, client)
		}
		return renderClient(stdout, client)
	default:
		return fmt.Errorf("client requires: enroll, issue-key, list, show, or revoke")
	}
}

func renderClient(output io.Writer, client controlauth.ClientRecord) error {
	text := fmt.Sprintf("Dorf Client %s\n  Name: %s\n  State: %s\n  Created: %s\n  Expires: %s\n",
		client.ID, client.Name, client.State, client.CreatedAt.Format(time.RFC3339), formatClientExpiry(client.ExpiresAt))
	if client.RevokedAt != nil {
		text += fmt.Sprintf("  Revoked: %s\n", client.RevokedAt.Format(time.RFC3339))
	}
	_, err := io.WriteString(output, text)
	return err
}

func formatClientExpiry(expiry *time.Time) string {
	if expiry == nil {
		return "never"
	}
	return expiry.Format(time.RFC3339)
}

func getOrRevokeClient(ctx context.Context, auth controlauth.Service, operation, id string) (controlauth.ClientRecord, error) {
	if operation == "revoke" {
		return auth.Revoke(ctx, id)
	}
	return auth.GetClient(ctx, id)
}
