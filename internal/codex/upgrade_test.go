package codex

import (
	"context"
	"reflect"
	"testing"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestUpgradeVerificationRequiresExactSettledNativeHistory(t *testing.T) {
	expected := []retainedTurn{{ThreadID: "thread", TurnID: "turn", TurnOutcome: "completed"}}
	for _, tc := range []struct {
		name  string
		turns []TurnOutcome
		valid bool
	}{
		{"retained", []TurnOutcome{{ID: "turn", Status: "completed"}}, true},
		{"empty", nil, false},
		{"different", []TurnOutcome{{ID: "other", Status: "completed"}}, false},
		{"active", []TurnOutcome{{ID: "turn", Status: "running"}}, false},
		{"changed", []TurnOutcome{{ID: "turn", Status: "failed"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyRetainedTurns(expected, tc.turns); (err == nil) != tc.valid {
				t.Fatalf("verification error=%v", err)
			}
		})
	}
}

func TestQuiesceNeverStartsOrStopsAnUnownedServer(t *testing.T) {
	for _, test := range []struct {
		probe   string
		wantErr bool
	}{
		{"0\n0\n", false}, // absent: no route credentials or native startup needed
		{"1\n0\n", true},  // untracked: not an owned kill target
		{"1\n1\n", true},  // tracked but missing authentication
	} {
		sandbox := quiesceProbeSandbox{t: t, probe: test.probe}
		if err := (Agent{Sandbox: sandbox}).Quiesce(context.Background(), testOwner("quiesce"), ""); (err != nil) != test.wantErr {
			t.Fatalf("probe %q: %v", test.probe, err)
		}
	}
}

type quiesceProbeSandbox struct {
	provider.Sandbox
	t     *testing.T
	probe string
}

func (s quiesceProbeSandbox) Endpoint(context.Context, provider.Ownership, int) (provider.Endpoint, error) {
	return provider.Endpoint{ListenURL: "ws://127.0.0.1:4500"}, nil
}

func (s quiesceProbeSandbox) Exec(_ context.Context, _ provider.Ownership, _ []byte, args ...string) (provider.Result, error) {
	if !reflect.DeepEqual(args, []string{"bash", "-lc", probeServerScript("ws://127.0.0.1:4500")}) {
		s.t.Fatalf("quiesce attempted an unexpected process operation: %q", args)
	}
	return provider.Result{Stdout: s.probe}, nil
}
