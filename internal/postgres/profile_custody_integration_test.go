package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

func TestIncusProfilePersistsExactEndpointAndGuestRouteDefinition(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("incus-custody-%d", time.Now().UnixNano())
	want := completeIncusProfile(name, "codex", strings.Repeat("c", 64))
	stored, created, err := store.CreateSandboxProfile(ctx, want)
	if err != nil || !created {
		t.Fatalf("created=%t profile=%#v err=%v", created, stored, err)
	}
	if stored.DefinitionHash == "" || stored.DefinitionHash != stored.CurrentDefinitionHash() || stored.BaseVerified() {
		t.Fatalf("stored profile=%#v", stored)
	}
	if stored.IncusEndpointAuthorityHash != want.IncusEndpointAuthorityHash || stored.IncusProject != want.IncusProject ||
		stored.IncusStoragePool != want.IncusStoragePool || stored.IncusNetwork != want.IncusNetwork ||
		stored.IncusGatewayURL != want.IncusGatewayURL {
		t.Fatalf("stored Incus definition=%#v want=%#v", stored, want)
	}

	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || verification.DefinitionHash != stored.DefinitionHash {
		t.Fatalf("verification=%#v err=%v", verification, err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, verification, "codex exact"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, verification); err != nil {
		t.Fatal(err)
	}
	verified, err := store.SandboxProfile(ctx, name)
	if err != nil || !verified.BaseVerified() {
		t.Fatalf("verified profile=%#v err=%v", verified, err)
	}
}

func TestIncusProfileAcceptsOnlyExactGuestReachableGatewayForms(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	for index, gateway := range []string{
		"http://127.0.0.1:8317/v1",
		"http://gateway.internal:8317/v1",
		"http://192.0.2.10:8317/v1",
		"http://[fd42::1]:8317/v1",
		"http://10.0.0.1:8317/v1?route=all",
		"https://gateway.example/v1/",
	} {
		profile := completeIncusProfile(fmt.Sprintf("invalid-gateway-%d-%d", index, time.Now().UnixNano()), "codex", strings.Repeat("d", 64))
		profile.IncusGatewayURL = gateway
		if _, _, err := store.CreateSandboxProfile(ctx, profile); err == nil || !strings.Contains(err.Error(), "Gateway URL") {
			t.Fatalf("Gateway %q error=%v", gateway, err)
		}
	}
	for index, gateway := range []string{
		"http://10.20.30.1:8317/v1",
		"http://100.64.0.1:8317/v1",
		"https://gateway.example/v1",
	} {
		profile := completeIncusProfile(fmt.Sprintf("valid-gateway-%d-%d", index, time.Now().UnixNano()), "codex", strings.Repeat("e", 64))
		profile.IncusGatewayURL = gateway
		if _, created, err := store.CreateSandboxProfile(ctx, profile); err != nil || !created {
			t.Fatalf("Gateway %q created=%t err=%v", gateway, created, err)
		}
	}
}

func TestIncusProfileRequiresCanonicalCompleteProviderDefinition(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	for index, test := range []struct {
		change func(*core.SandboxProfile)
		want   string
	}{
		{change: func(p *core.SandboxProfile) { p.IncusEndpointAuthorityHash = "" }, want: "authority hash"},
		{change: func(p *core.SandboxProfile) { p.IncusProject = "Dorf" }, want: "project"},
		{change: func(p *core.SandboxProfile) { p.IncusStoragePool = "fast_pool" }, want: "storage pool"},
		{change: func(p *core.SandboxProfile) { p.IncusNetwork = "incus_br0" }, want: "network"},
		{change: func(p *core.SandboxProfile) { p.IncusDiskSize = "40GB" }, want: "disk size"},
		{change: func(p *core.SandboxProfile) { p.IncusGatewayURL = "" }, want: "Gateway URL"},
	} {
		profile := completeIncusProfile(fmt.Sprintf("invalid-definition-%d-%d", index, time.Now().UnixNano()), "codex", strings.Repeat("7", 64))
		test.change(&profile)
		if _, _, err := store.CreateSandboxProfile(ctx, profile); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("definition %d error=%v, want %q", index, err, test.want)
		}
	}
}

func TestAdmissionRequiresVerificationHashToMatchCurrentProfileDefinition(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("hash-fence-%d", time.Now().UnixNano())
	profile, _, err := store.CreateSandboxProfile(ctx, completeIncusProfile(name, "codex", strings.Repeat("f", 64)))
	if err != nil {
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex exact")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.sandbox_profile_verifications set definition_hash=$2 where profile_name=$1`, name, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	profile, err = store.SandboxProfile(ctx, name)
	if err != nil || profile.BaseVerified() {
		t.Fatalf("mismatched profile=%#v err=%v", profile, err)
	}
	_, _, err = store.AdmitDirect(ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("hash-fence-admission-%d", time.Now().UnixNano()),
		Goal:         "prove the profile hash fence", SandboxProfile: name, ProviderConnection: "primary",
		Model: "gpt-5.6-sol", ReasoningEffort: "high",
	})
	if err == nil || !strings.Contains(err.Error(), core.BaseProfileContract) {
		t.Fatalf("admission error=%v", err)
	}
}

func completeIncusProfile(name, harness, artifact string) core.SandboxProfile {
	return core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderIncus, Harness: harness, Artifact: artifact,
		IncusEndpointAuthorityHash: strings.Repeat("b", 64), IncusProject: "dorf", IncusStoragePool: "default",
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB", IncusGatewayURL: "http://10.20.30.1:8317/v1",
	}
}
