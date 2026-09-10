package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/postgres"
)

type controlAPIProfiles struct {
	store postgres.Store
}

func (a controlAPIProfiles) List(ctx context.Context) (controlapi.ProfileList, error) {
	profiles, err := a.store.SandboxProfiles(ctx)
	if err != nil {
		return controlapi.ProfileList{}, err
	}
	list := controlapi.ProfileList{Profiles: make([]controlapi.ProfileSummary, 0, len(profiles))}
	for _, profile := range profiles {
		list.Profiles = append(list.Profiles, controlapi.ProfileSummary{
			Name: profile.Name, Provider: string(profile.Provider), Harness: profile.Harness,
			Default: profile.Default, Verified: profile.BaseVerified(),
		})
	}
	return list, nil
}

func remoteProfileList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("profile list", flag.ContinueOnError)
	set.SetOutput(stderr)
	output := set.String("output", "human", "output format: human or json")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("profile list does not accept positional arguments")
	}
	if err := validateOutput(*output); err != nil {
		return err
	}
	cfg, _, client, err := loadConnectedClient()
	if err != nil {
		return err
	}
	list, err := client.ListProfiles(ctx)
	if err != nil {
		return jobControlError(cfg.DeploymentURL, err)
	}
	if *output == "json" {
		return writeJSON(stdout, list)
	}
	if len(list.Profiles) == 0 {
		_, err = fmt.Fprintln(stdout, "No Sandbox profiles found")
		return err
	}
	for _, profile := range list.Profiles {
		if _, err := fmt.Fprintf(stdout, "%s  provider=%s  harness=%s  default=%t  verified=%t\n",
			profile.Name, profile.Provider, profile.Harness, profile.Default, profile.Verified); err != nil {
			return err
		}
	}
	return nil
}
