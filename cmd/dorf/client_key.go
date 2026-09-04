package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/postgres"
)

func issueClientKey(ctx context.Context, store postgres.Store, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("client issue-key", flag.ContinueOnError)
	set.SetOutput(stderr)
	name := set.String("name", "", "Client name")
	path := set.String("credential-file", "", "new owner-only file for the bearer credential")
	noExpiry := set.Bool("no-expiry", false, "explicitly issue a credential that remains valid until revoked")
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 || strings.TrimSpace(*name) == "" || strings.TrimSpace(*path) == "" || !*noExpiry {
		return fmt.Errorf("client issue-key requires --name NAME --credential-file PATH --no-expiry")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		return err
	}
	if err := writeNewCredentialFile(*path, credential); err != nil {
		return err
	}
	client, err := (controlauth.Service{Store: store}).IssueKey(ctx, *name, credential)
	if err != nil {
		// Retain the protected proof if the database outcome is uncertain.
		return fmt.Errorf("issue Client key failed; protected credential retained at %s: %w", *path, err)
	}
	if *output == "json" {
		err = writeJSON(stdout, client)
	} else {
		err = renderClient(stdout, client)
	}
	if err != nil {
		return fmt.Errorf("Client %s already issued; protected credential saved at %s; receipt output failed: %w", client.ID, *path, err)
	}
	return nil
}

func writeNewCredentialFile(path, credential string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create credential file: %w", err)
	}
	if _, err := io.WriteString(file, credential+"\n"); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf("sync credential file: %w", err)
	}
	return file.Close()
}
