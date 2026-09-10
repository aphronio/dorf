package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
)

func TestControlAPIProfileDiscoveryAndSelection(t *testing.T) {
	ctx := context.Background()
	store, tasks, defaultName := controlTestStore(t)
	cloudName := fmt.Sprintf("cloud-discovery-%d", time.Now().UnixNano())
	cloud, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: cloudName, Provider: core.SandboxProviderE2B, Harness: "codex",
		Artifact: "private-template:private-build", E2BGatewayURL: "https://private-gateway.example.test/v1",
		E2BSandboxTimeout: time.Hour, E2BAllowInternet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := controlauth.Service{Store: store}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.IssueKey(ctx, "profile-discovery", credential); err != nil {
		t.Fatal(err)
	}
	handler := controlTestHandler(store, tasks, controlTestGateway(t), auth, nil, blob.Store{Root: t.TempDir()})
	read := func() map[string]controlapi.ProfileSummary {
		t.Helper()
		response := controlTestRequest(t, handler, http.MethodGet, "/v1/profiles", credential, "", nil)
		var list controlapi.ProfileList
		controlTestJSON(t, response, http.StatusOK, &list)
		names := make([]string, 0, len(list.Profiles))
		profiles := make(map[string]controlapi.ProfileSummary, len(list.Profiles))
		for _, profile := range list.Profiles {
			names = append(names, profile.Name)
			profiles[profile.Name] = profile
		}
		if !sort.StringsAreSorted(names) {
			t.Fatalf("profile names are not sorted: %v", names)
		}
		var raw struct {
			Profiles []map[string]json.RawMessage `json:"profiles"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		for _, profile := range raw.Profiles {
			if len(profile) != 5 {
				t.Fatalf("profile has unexpected fields: %v", profile)
			}
			for _, key := range []string{"name", "provider", "harness", "default", "verified"} {
				if _, ok := profile[key]; !ok {
					t.Fatalf("profile omitted %q: %v", key, profile)
				}
			}
		}
		for _, private := range []string{cloud.Artifact, cloud.E2BGatewayURL, "10.44.0.1"} {
			if strings.Contains(response.Body.String(), private) {
				t.Fatalf("profile response disclosed %q", private)
			}
		}
		return profiles
	}
	profiles := read()
	if got := profiles[defaultName]; got != (controlapi.ProfileSummary{Name: defaultName, Provider: "incus", Harness: "codex", Default: true, Verified: true}) {
		t.Fatalf("default profile=%+v", got)
	}
	if got := profiles[cloudName]; got != (controlapi.ProfileSummary{Name: cloudName, Provider: "e2b", Harness: "codex"}) {
		t.Fatalf("unverified cloud profile=%+v", got)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, cloudName)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex-test")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}
	profiles = read()
	if !profiles[cloudName].Verified {
		t.Fatal("completed verification did not become visible")
	}
	key := fmt.Sprintf("profile-discovery-job-%d", time.Now().UnixNano())
	response := controlTestRequest(t, handler, http.MethodPost, "/v1/jobs", credential, key, controlapi.AdmitJobRequest{
		Profile: profiles[cloudName].Name, AIConnection: "primary", Model: "model-test",
	})
	var job controlapi.DirectJob
	controlTestJSON(t, response, http.StatusCreated, &job)
	if job.Profile != cloudName {
		t.Fatalf("admitted profile=%q, want discovered %q", job.Profile, cloudName)
	}
	// Refreshing proof invalidates new admission eligibility without erasing the profile.
	if _, _, err := store.BeginSandboxProfileVerification(ctx, cloudName); err != nil {
		t.Fatal(err)
	}
	if read()[cloudName].Verified {
		t.Fatal("incomplete verification was reported as verified")
	}
}

func TestControlAPIUnknownProfilesHaveASpecificProblem(t *testing.T) {
	ctx := context.Background()
	store, tasks, _ := controlTestStore(t)
	auth := controlauth.Service{Store: store}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.IssueKey(ctx, "unknown-profile", credential); err != nil {
		t.Fatal(err)
	}
	handler := controlTestHandler(store, tasks, controlTestGateway(t), auth, nil, blob.Store{Root: t.TempDir()})
	missing := fmt.Sprintf("missing-profile-%d", time.Now().UnixNano())
	for _, test := range []struct {
		path  string
		input any
	}{
		{"/v1/jobs", controlapi.AdmitJobRequest{Profile: missing}},
		{"/v1/workflows/coding/jobs", controlapi.AdmitCodingJobRequest{
			Profile: missing, Repository: "https://github.com/acme/widget.git", Revision: strings.Repeat("a", 40), BaseBranch: "main",
		}},
		{"/v1/workflows/codebase-investigation/jobs", controlapi.AdmitInvestigationJobRequest{
			Profile: missing, Repository: "https://github.com/acme/widget.git", Revision: strings.Repeat("a", 40),
		}},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := controlTestRequest(t, handler, http.MethodPost, test.path, credential,
				fmt.Sprintf("missing-profile-request-%d", time.Now().UnixNano()), test.input)
			var problem controlapi.Problem
			controlTestJSON(t, response, http.StatusUnprocessableEntity, &problem)
			if problem.Code != "profile_not_found" || !strings.Contains(problem.Title, "GET /v1/profiles") {
				t.Fatalf("unknown profile Problem=%+v", problem)
			}
		})
	}
}
