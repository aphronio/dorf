package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aphronio/dorf/internal/config"
	githubapi "github.com/aphronio/dorf/internal/github"
)

func integrationCommand(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "github" {
		return fmt.Errorf("integration requires github setup")
	}
	if args[1] != "setup" {
		return fmt.Errorf("unsupported GitHub integration command %q", args[1])
	}
	client := githubapi.Client{APIURL: cfg.GitHubAPIURL, Credentials: cfg.GitHubCredentials}
	return githubIntegrationSetup(ctx, client, args[2:], os.Stdin, stdout, stderr)
}

func githubIntegrationSetup(ctx context.Context, client githubapi.Client, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("integration github setup", flag.ContinueOnError)
	set.SetOutput(stderr)
	organization := set.String("org", "", "create an organization-owned App for this exact owner")
	yes := set.Bool("yes", false, "replace the configured deployment App")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("integration github setup does not accept positional arguments")
	}

	configured, present, configuredErr := client.ConfiguredApp(ctx)
	if configuredErr != nil && (!present || !*yes) {
		return configuredErr
	}
	if present && !*yes {
		printGitHubAppReady(stdout, configured)
		return nil
	}

	approval, err := client.ManifestApproval(githubapi.ManifestInput{Organization: strings.TrimSpace(*organization)})
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Create the Dorf GitHub App in your browser:")
	writeTerminalLink(stdout, "Open GitHub App setup", approval.URL)
	fmt.Fprintf(stdout, "If the link is not clickable or did not open, copy and paste this address into your browser:\n%s\n", approval.URL)
	fmt.Fprintln(stdout, "\nAfter approval, paste the redirected URL or short-lived manifest code and press Enter:")
	reader := bufio.NewReader(stdin)
	// TODO: When a Dorf web UI or cloud control plane exists, replace this manual handoff with its authenticated callback.
	handoff, err := readGitHubSetupLine(reader)
	if err != nil {
		return fmt.Errorf("read GitHub manifest handoff: %w", err)
	}
	code, err := githubapi.ParseManifestCode(handoff, approval.State)
	if err != nil {
		return err
	}
	converted, err := client.ConvertManifest(ctx, code, approval.Owner, *yes)
	if err != nil {
		if errors.Is(err, githubapi.ErrCredentialReplacementRequiresApproval) {
			return fmt.Errorf("different GitHub App credentials are already configured; rerun with --yes to replace them")
		}
		return err
	}
	printGitHubAppReady(stdout, converted)
	return nil
}

func writeTerminalLink(output io.Writer, label, target string) {
	fmt.Fprintf(output, "\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\\n", target, label)
}

func printGitHubAppReady(stdout io.Writer, app githubapi.ConvertedApp) {
	fmt.Fprintf(stdout, "GitHub App configured\n  Install or manage repository access: %s\n", app.InstallURL)
}

func readGitHubSetupLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", fmt.Errorf("input was empty")
	}
	return line, nil
}
