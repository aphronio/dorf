package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
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

func TestLegacyProfileLoadsIncompleteUntilExplicitUpdateAndReverification(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("legacy-incus-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `
insert into dorf.sandbox_profiles(name,provider,harness,artifact,incus_network,incus_disk_size)
values($1,'incus','codex',$2,'incusbr0','40GiB')`, name, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.sandbox_profile_verifications(
    profile_name,contract_version,sandbox_id,ownership_nonce,harness_version,probe_completed_at,cleaned_at
	) values($1,$2,$3,$4,'legacy-codex',clock_timestamp(),clock_timestamp())`,
		name, core.BaseProfileContract, "sandbox-"+name, strings.Repeat("9", 64)); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.SandboxProfile(ctx, name)
	if err != nil || legacy.DefinitionHash != "" || legacy.BaseVerified() {
		t.Fatalf("legacy profile=%#v err=%v", legacy, err)
	}
	if _, _, err := store.BeginSandboxProfileVerification(ctx, name); err == nil || !strings.Contains(err.Error(), "explicitly") {
		t.Fatalf("legacy verification error=%v", err)
	}
	project, pool, gateway, authority := "dorf", "default", "http://10.20.30.1:8317/v1", strings.Repeat("b", 64)
	updated, changed, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{
		IncusEndpointAuthorityHash: &authority,
		IncusProject:               &project,
		IncusStoragePool:           &pool,
		IncusGatewayURL:            &gateway,
	})
	if err != nil || !changed || updated.DefinitionHash == "" || updated.Verification != nil || updated.BaseVerified() {
		t.Fatalf("updated=%#v changed=%t err=%v", updated, changed, err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || verification.DefinitionHash != updated.DefinitionHash {
		t.Fatalf("verification=%#v err=%v", verification, err)
	}
}

func TestLegacyProfileAdoptionRestoresInFlightJobsAndReplaysConcurrently(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("legacy-active-incus-%d", time.Now().UnixNano())
	desired := completeIncusProfile(name, "codex", strings.Repeat("6", 64))
	if _, created, err := store.CreateSandboxProfile(ctx, desired); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex legacy")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}
	job, created, err := store.AdmitDirect(ctx, core.JobAdmission{
		AdmissionKey: "legacy-active-" + name, Goal: "recover after profile custody migration",
		SandboxProfile: name, ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
	})
	if err != nil || !created {
		t.Fatalf("admitted Job=%#v created=%t err=%v", job, created, err)
	}
	if _, err := db.ExecContext(ctx, `
update dorf.sandbox_profiles
set definition_hash=null,incus_endpoint_authority_hash=null,incus_project=null,
    incus_storage_pool=null,incus_gateway_url=null
where name=$1`, name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.sandbox_profile_verifications set definition_hash=null where profile_name=$1`, name); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.SandboxProfile(ctx, name)
	if err != nil || legacy.DefinitionHash != "" || legacy.BaseVerified() {
		t.Fatalf("legacy profile=%#v err=%v", legacy, err)
	}

	type adoption struct {
		profile core.SandboxProfile
		changed bool
		err     error
	}
	const attempts = 8
	results := make(chan adoption, attempts)
	var started sync.WaitGroup
	started.Add(attempts)
	release := make(chan struct{})
	for range attempts {
		go func() {
			started.Done()
			<-release
			profile, changed, err := store.AdoptLegacySandboxProfile(ctx, desired)
			results <- adoption{profile: profile, changed: changed, err: err}
		}()
	}
	started.Wait()
	close(release)
	changed := 0
	for range attempts {
		result := <-results
		if result.err != nil || result.profile.DefinitionHash != result.profile.CurrentDefinitionHash() {
			t.Fatalf("adoption=%#v", result)
		}
		if result.changed {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("changed adoptions=%d want 1", changed)
	}
	adopted, err := store.SandboxProfile(ctx, name)
	if err != nil || adopted.Verification != nil || adopted.BaseVerified() || adopted.Default {
		t.Fatalf("adopted profile=%#v err=%v", adopted, err)
	}
	retainedJob, err := store.Job(ctx, job.ID)
	if err != nil || retainedJob.ID != job.ID || retainedJob.CleanupState == core.CleanupComplete {
		t.Fatalf("retained Job=%#v err=%v", retainedJob, err)
	}
	_, freshVerification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || freshVerification.DefinitionHash != adopted.DefinitionHash {
		t.Fatalf("fresh verification=%#v err=%v", freshVerification, err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, freshVerification, "codex adopted"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, freshVerification); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
update dorf.jobs set admission_open=false,cleanup_state='complete',cleaned_at=clock_timestamp()
where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyProfileAdoptionRetainsInterruptedVerificationThenRequiresFreshProof(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("legacy-interrupted-incus-%d", time.Now().UnixNano())
	desired := completeIncusProfile(name, "codex", strings.Repeat("5", 64))
	if _, created, err := store.CreateSandboxProfile(ctx, desired); err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	_, interrupted, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
update dorf.sandbox_profiles
set definition_hash=null,incus_endpoint_authority_hash=null,incus_project=null,
    incus_storage_pool=null,incus_gateway_url=null
where name=$1`, name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.sandbox_profile_verifications set definition_hash=null where profile_name=$1`, name); err != nil {
		t.Fatal(err)
	}

	adopted, changed, err := store.AdoptLegacySandboxProfile(ctx, desired)
	if err != nil || !changed || adopted.Verification == nil {
		t.Fatalf("adopted=%#v changed=%t err=%v", adopted, changed, err)
	}
	retained := *adopted.Verification
	if retained.SandboxID != interrupted.SandboxID || retained.OwnershipNonce != interrupted.OwnershipNonce ||
		retained.DefinitionHash != adopted.DefinitionHash || retained.ContractVersion == core.BaseProfileContract {
		t.Fatalf("retained verification=%#v interrupted=%#v", retained, interrupted)
	}
	_, resumed, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || resumed.SandboxID != interrupted.SandboxID || resumed.OwnershipNonce != interrupted.OwnershipNonce {
		t.Fatalf("resumed verification=%#v err=%v", resumed, err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, resumed, "codex resumed"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, resumed); err != nil {
		t.Fatal(err)
	}
	cleanedLegacy, err := store.SandboxProfile(ctx, name)
	if err != nil || cleanedLegacy.BaseVerified() || cleanedLegacy.Verification == nil || cleanedLegacy.Verification.CleanedAt.IsZero() {
		t.Fatalf("cleaned legacy verification profile=%#v err=%v", cleanedLegacy, err)
	}
	_, fresh, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || fresh.ContractVersion != core.BaseProfileContract || fresh.DefinitionHash != adopted.DefinitionHash ||
		fresh.OwnershipNonce == interrupted.OwnershipNonce {
		t.Fatalf("fresh verification=%#v err=%v", fresh, err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, fresh, "codex fresh"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	verified, err := store.SandboxProfile(ctx, name)
	if err != nil || !verified.BaseVerified() {
		t.Fatalf("freshly verified profile=%#v err=%v", verified, err)
	}
}

func TestLegacyProfileAdoptionCannotMutateAnExistingCompleteDefinition(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("nonlegacy-adoption-%d", time.Now().UnixNano())
	original := completeIncusProfile(name, "codex", strings.Repeat("3", 64))
	stored, created, err := store.CreateSandboxProfile(ctx, original)
	if err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	changed := original
	changed.IncusGatewayURL = "http://10.20.30.2:8317/v1"
	if _, _, err := store.AdoptLegacySandboxProfile(ctx, changed); err == nil || !strings.Contains(err.Error(), "not the same") {
		t.Fatalf("complete definition adoption error=%v", err)
	}
	after, err := store.SandboxProfile(ctx, name)
	if err != nil || after.DefinitionHash != stored.DefinitionHash || after.IncusGatewayURL != stored.IncusGatewayURL {
		t.Fatalf("profile after rejected adoption=%#v err=%v", after, err)
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
