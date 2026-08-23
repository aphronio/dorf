package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/aphronio/dorf/internal/config"
	githubapi "github.com/aphronio/dorf/internal/github"
)

func integrationCommand(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 || args[0] != "github" {
		return fmt.Errorf("integration requires github setup or github verify")
	}
	switch args[1] {
	case "setup":
		return githubIntegrationSetup(ctx, cfg, args[2:], stdout, stderr)
	case "verify":
		return githubIntegrationVerify(ctx, cfg, args[2:], stdout, stderr)
	default:
		return fmt.Errorf("unsupported GitHub integration command %q", args[1])
	}
}

func githubIntegrationSetup(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("integration github setup", flag.ContinueOnError)
	set.SetOutput(stderr)
	appID := set.String("app-id", "", "positive decimal GitHub App ID")
	privateKey := set.String("private-key", "", "path to the GitHub App RSA private key")
	yes := set.Bool("yes", false, "approve replacement of different installed credentials")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("integration github setup does not accept positional arguments")
	}

	presenter := newSetupPresenter(stdout)
	if strings.TrimSpace(*appID) == "" || strings.TrimSpace(*privateKey) == "" {
		if !presenter.interactive {
			return fmt.Errorf("--app-id and --private-key are required for non-interactive setup")
		}
		if err := presenter.RunForm(ctx,
			presenter.TextGroup("GitHub App ID", "Positive decimal App ID", "123456", appID, nil),
			presenter.TextGroup("GitHub App private key", "Path to the downloaded RSA PEM file", "./app.private-key.pem", privateKey, nil),
		); err != nil {
			return err
		}
	}
	app, key := strings.TrimSpace(*appID), strings.TrimSpace(*privateKey)
	client := githubapi.Client{APIURL: cfg.GitHubAPIURL, Credentials: cfg.GitHubCredentials}
	input := githubapi.SetupInput{AppID: app, SourcePrivateKey: key, ReplaceCredentials: *yes}
	err := client.Setup(ctx, input)
	if errors.Is(err, githubapi.ErrCredentialReplacementRequiresApproval) && presenter.interactive && !*yes {
		approved := false
		if promptErr := presenter.RunForm(ctx, presenter.ConfirmGroup("Replace GitHub App credentials?", "The existing integration will use the new App identity.", &approved)); promptErr != nil {
			return promptErr
		}
		if !approved {
			return fmt.Errorf("GitHub integration setup cancelled")
		}
		input.ReplaceCredentials = true
		err = client.Setup(ctx, input)
	}
	if err != nil {
		if errors.Is(err, githubapi.ErrCredentialReplacementRequiresApproval) {
			return fmt.Errorf("different GitHub App credentials are already configured; rerun with --yes to replace them")
		}
		return err
	}
	fmt.Fprintf(stdout, "GitHub App credentials ready\n  App ID: %s\n  Credentials: %s\nNext: dorf integration github verify --repo OWNER/REPOSITORY --installation INSTALLATION_ID\n", app, cfg.GitHubCredentials)
	return nil
}

func githubIntegrationVerify(ctx context.Context, cfg config.Config, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("integration github verify", flag.ContinueOnError)
	set.SetOutput(stderr)
	repository := set.String("repo", "", "canonical lower-case owner/repository")
	installation := set.String("installation", "", "GitHub App installation identity")
	base := set.String("base", "", "optional exact base branch")
	requirements := map[string]string{}
	set.Func("require", "native GitHub permission NAME:LEVEL; repeat as needed", func(raw string) error {
		name, level, found := strings.Cut(strings.TrimSpace(raw), ":")
		if !found || name == "" || level == "" || strings.Contains(level, ":") {
			return fmt.Errorf("GitHub requirement must be NAME:LEVEL")
		}
		if existing, found := requirements[name]; found && existing != level {
			return fmt.Errorf("GitHub permission %s was required at two levels", name)
		}
		requirements[name] = level
		return nil
	})
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("integration github verify does not accept positional arguments")
	}
	repo, install, branch := strings.TrimSpace(*repository), strings.TrimSpace(*installation), strings.TrimSpace(*base)
	if repo == "" || install == "" {
		return fmt.Errorf("--repo and --installation are required together")
	}
	client := githubapi.Client{APIURL: cfg.GitHubAPIURL, Credentials: cfg.GitHubCredentials}
	revision, verified, err := client.Verify(ctx, githubapi.Authority{Repository: repo, InstallationID: install}, branch, requirements)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(verified))
	for name := range verified {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		names[i] = name + ":" + verified[name]
	}
	fmt.Fprintf(stdout, "GitHub integration verified\n  Repository: %s\n  Installation: %s\n  Permissions: %s\n", repo, install, strings.Join(names, ", "))
	if branch != "" {
		fmt.Fprintf(stdout, "  Base: %s\n  Revision: %s\n", branch, revision)
	}
	return nil
}
