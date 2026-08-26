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
		return fmt.Errorf("client requires: enroll, list, show, or revoke")
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
			fmt.Fprintf(stdout, "  %s  %s  %s  expires %s\n", client.ID, client.Name, client.State, client.ExpiresAt.Format(time.RFC3339))
		}
		return nil
	case "show", "revoke":
		if set.NArg() != 1 {
			return fmt.Errorf("client %s requires one Client ID", args[0])
		}
		var client controlauth.ClientRecord
		var err error
		if args[0] == "show" {
			client, err = auth.GetClient(ctx, set.Arg(0))
		} else {
			client, err = auth.Revoke(ctx, set.Arg(0))
		}
		if err != nil {
			return err
		}
		if *output == "json" {
			return writeJSON(stdout, client)
		}
		renderClient(stdout, client)
		return nil
	default:
		return fmt.Errorf("client requires: enroll, list, show, or revoke")
	}
}

func renderClient(output io.Writer, client controlauth.ClientRecord) {
	fmt.Fprintf(output, "Dorf Client %s\n  Name: %s\n  State: %s\n  Created: %s\n  Expires: %s\n",
		client.ID, client.Name, client.State, client.CreatedAt.Format(time.RFC3339), client.ExpiresAt.Format(time.RFC3339))
	if client.RevokedAt != nil {
		fmt.Fprintf(output, "  Revoked: %s\n", client.RevokedAt.Format(time.RFC3339))
	}
}
