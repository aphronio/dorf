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
	reader := bufio.NewReader(stdin)

	configured, present, configuredErr := client.ConfiguredApp(ctx)
	if configuredErr != nil && (!present || !*yes) {
		return configuredErr
	}
	if present && !*yes {
		installed, err := client.HasInstallation(ctx)
		if err != nil {
			return err
		}
		if installed {
			printGitHubIntegrationReady(stdout)
			return nil
		}
		fmt.Fprintln(stdout, "GitHub App already configured")
		return finishGitHubInstallation(ctx, client, configured, reader, stdout)
	}

	approval, err := client.ManifestApproval(githubapi.ManifestInput{Organization: strings.TrimSpace(*organization)})
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Create the Dorf GitHub App in your browser:")
	writeTerminalLink(stdout, "Open GitHub App setup", approval.URL)
	fmt.Fprintf(stdout, "If the link is not clickable or did not open, copy and paste this address into your browser:\n%s\n", approval.URL)
	fmt.Fprintln(stdout, "\nAfter approval, the page will show a one-time code. Copy it, paste it here, and press Enter:")
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
	fmt.Fprintln(stdout, "GitHub App created")
	return finishGitHubInstallation(ctx, client, converted, reader, stdout)
}

func writeTerminalLink(output io.Writer, label, target string) {
	fmt.Fprintf(output, "\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\\n", target, label)
}

func finishGitHubInstallation(ctx context.Context, client githubapi.Client, app githubapi.ConvertedApp, reader *bufio.Reader, stdout io.Writer) error {
	fmt.Fprintln(stdout, "Install repository access for the Dorf GitHub App in your browser:")
	writeTerminalLink(stdout, "Open GitHub App installation", app.InstallURL)
	fmt.Fprintf(stdout, "If the link is not clickable or did not open, copy and paste this address into your browser:\n%s\n", app.InstallURL)
	fmt.Fprintln(stdout, "\nAfter GitHub reports that the App was installed, type installed and press Enter:")
	confirmation, err := readGitHubSetupLine(reader)
	if err != nil {
		return fmt.Errorf("read GitHub installation confirmation: %w", err)
	}
	if confirmation != "installed" {
		return fmt.Errorf("GitHub installation confirmation must be exactly installed")
	}
	installed, err := client.HasInstallation(ctx)
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("GitHub App has no installation after confirmation; install repository access at %s and rerun dorf integration github setup", app.InstallURL)
	}
	fmt.Fprintln(stdout, "GitHub repository access installed")
	printGitHubIntegrationReady(stdout)
	return nil
}

func printGitHubIntegrationReady(stdout io.Writer) {
	fmt.Fprintln(stdout, "GitHub integration ready")
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
