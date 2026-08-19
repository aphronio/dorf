package hostsetup

import (
	"reflect"
	"strings"
	"testing"
)

func TestDeriveHostPlanContainsOnlyObservedMissingChanges(t *testing.T) {
	plan, err := deriveHostPlan(hostObservation{
		username: "alice", ubuntu2404: true,
		dockerCommand: true, dockerService: true, dockerGroup: true, dockerAccess: true,
		incusService: true, incusGroup: true, kvmAccess: true,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Install Incus", "Install QEMU"}
	if !reflect.DeepEqual(plan.Summaries(), want) {
		t.Fatalf("summaries=%v want=%v", plan.Summaries(), want)
	}
	if plan.Description() != "  • Install Incus\n  • Install QEMU" {
		t.Fatalf("description=%q", plan.Description())
	}
}

func TestDeriveHostPlanFreshUbuntuIncludesEveryRequiredAuthority(t *testing.T) {
	plan, err := deriveHostPlan(hostObservation{username: "alice", ubuntu2404: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Install Docker Engine", "Enable and start Docker", "Grant alice root-equivalent Docker access",
		"Install Incus", "Enable and start Incus", "Grant alice root-equivalent Incus access", "Install QEMU",
		"Grant alice access to hardware virtualization",
	}
	if !reflect.DeepEqual(plan.Summaries(), want) {
		t.Fatalf("summaries=%v want=%v", plan.Summaries(), want)
	}
	if !plan.needsRelogin {
		t.Fatal("group changes must require a fresh login")
	}
}

func TestDeriveHostPlanRefusesMutationOutsideSupportedUbuntu(t *testing.T) {
	_, err := deriveHostPlan(hostObservation{username: "alice"}, true)
	if err == nil || !strings.Contains(err.Error(), "Ubuntu 24.04") {
		t.Fatalf("error=%v", err)
	}
}

func TestDeriveHostPlanAcceptsReadyNonUbuntuHost(t *testing.T) {
	plan, err := deriveHostPlan(hostObservation{
		username:      "alice",
		dockerCommand: true, dockerService: true, dockerGroup: true, dockerAccess: true,
		incusCommand: true, incusService: true, incusGroup: true, incusAccess: true,
		qemuCommand: true, kvmAccess: true,
	}, true)
	if err != nil || !plan.Empty() {
		t.Fatalf("plan=%#v error=%v", plan, err)
	}
}

func TestDeriveHostPlanReportsStaleGroupAccess(t *testing.T) {
	_, err := deriveHostPlan(hostObservation{
		username:      "alice",
		dockerCommand: true, dockerService: true, dockerGroup: true,
		incusCommand: true, incusService: true, incusGroup: true,
		qemuCommand: true, kvmAccess: true,
	}, true)
	if err == nil || !strings.Contains(err.Error(), "sign out and back in") {
		t.Fatalf("error=%v", err)
	}
}
