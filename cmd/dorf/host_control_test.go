package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlclient"
	"github.com/aphronio/dorf/internal/hostclientconfig"
)

type hostEnrollmentStub struct {
	enrollment controlauth.Enrollment
	calls      int
}

func (s *hostEnrollmentStub) CreateEnrollment(context.Context) (controlauth.Enrollment, error) {
	s.calls++
	return s.enrollment, nil
}

type hostClientStub struct {
	me          []hostClientResult
	meCalls     int
	redeem      hostClientResult
	redeemCalls int
}

type hostClientResult struct {
	identity controlapi.Identity
	err      error
}

func (c *hostClientStub) Me(context.Context) (controlapi.Identity, error) {
	c.meCalls++
	result := c.me[0]
	c.me = c.me[1:]
	return result.identity, result.err
}

func (c *hostClientStub) RedeemEnrollment(context.Context, string, string) (controlapi.Identity, error) {
	c.redeemCalls++
	return c.redeem.identity, c.redeem.err
}

func TestHostControlClientReconciliationConvergesWithoutRotatingOnAPIOutage(t *testing.T) {
	identity := controlapi.Identity{Client: controlapi.Client{ID: "cli_host"}}
	unauthenticated := &controlclient.ProblemError{Problem: controlapi.Problem{Status: 401, Code: "unauthenticated"}}

	t.Run("valid credential", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state", "host-client.json")
		if err := hostclientconfig.Save(path, hostclientconfig.Config{Credential: "retained"}); err != nil {
			t.Fatal(err)
		}
		enrollments := &hostEnrollmentStub{}
		client := &hostClientStub{me: []hostClientResult{{identity: identity}}}
		generated := false
		got, err := reconcileHostControlClient(context.Background(), enrollments, path,
			func() (string, error) { generated = true; return "replacement", nil },
			func(string) (hostControlClient, error) { return client, nil })
		if err != nil || got != identity || generated || enrollments.calls != 0 || client.meCalls != 1 || client.redeemCalls != 0 {
			t.Fatalf("identity=%#v generated=%t enrollments=%d me=%d redeems=%d err=%v", got, generated, enrollments.calls, client.meCalls, client.redeemCalls, err)
		}
	})

	t.Run("fresh enrollment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state", "host-client.json")
		enrollments := &hostEnrollmentStub{enrollment: controlauth.Enrollment{Token: "enrollment"}}
		client := &hostClientStub{redeem: hostClientResult{identity: identity}}
		got, err := reconcileHostControlClient(context.Background(), enrollments, path,
			func() (string, error) { return "credential", nil },
			func(string) (hostControlClient, error) { return client, nil })
		if err != nil || got != identity || enrollments.calls != 1 || client.meCalls != 0 || client.redeemCalls != 1 {
			t.Fatalf("identity=%#v enrollments=%d me=%d redeems=%d err=%v", got, enrollments.calls, client.meCalls, client.redeemCalls, err)
		}
		stored, found, err := hostclientconfig.Load(path)
		if err != nil || !found || stored.Credential != "credential" || stored.EnrollmentCode != "" {
			t.Fatalf("stored=%#v found=%t err=%v", stored, found, err)
		}
	})

	t.Run("interrupted enrollment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state", "host-client.json")
		if err := hostclientconfig.Save(path, hostclientconfig.Config{Credential: "candidate", EnrollmentCode: "pending"}); err != nil {
			t.Fatal(err)
		}
		enrollments := &hostEnrollmentStub{}
		client := &hostClientStub{
			me:     []hostClientResult{{err: unauthenticated}},
			redeem: hostClientResult{identity: identity},
		}
		generated := false
		got, err := reconcileHostControlClient(context.Background(), enrollments, path,
			func() (string, error) { generated = true; return "replacement", nil },
			func(string) (hostControlClient, error) { return client, nil })
		if err != nil || got != identity || generated || enrollments.calls != 0 || client.meCalls != 1 || client.redeemCalls != 1 {
			t.Fatalf("identity=%#v generated=%t enrollments=%d me=%d redeems=%d err=%v", got, generated, enrollments.calls, client.meCalls, client.redeemCalls, err)
		}
	})

	t.Run("API outage", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state", "host-client.json")
		if err := hostclientconfig.Save(path, hostclientconfig.Config{Credential: "retained"}); err != nil {
			t.Fatal(err)
		}
		enrollments := &hostEnrollmentStub{}
		client := &hostClientStub{me: []hostClientResult{{err: errors.New("connection refused")}}}
		generated := false
		_, err := reconcileHostControlClient(context.Background(), enrollments, path,
			func() (string, error) { generated = true; return "replacement", nil },
			func(string) (hostControlClient, error) { return client, nil })
		if err == nil || generated || enrollments.calls != 0 || client.meCalls != 1 || client.redeemCalls != 0 {
			t.Fatalf("generated=%t enrollments=%d me=%d redeems=%d err=%v", generated, enrollments.calls, client.meCalls, client.redeemCalls, err)
		}
		stored, _, loadErr := hostclientconfig.Load(path)
		if loadErr != nil || stored.Credential != "retained" {
			t.Fatalf("stored=%#v err=%v", stored, loadErr)
		}
	})
}
