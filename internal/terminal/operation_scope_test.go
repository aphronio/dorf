package terminal

import (
	"context"
	"errors"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type mismatchScopeHarness struct {
	Harness
	entered bool
}

func (*mismatchScopeHarness) Name() string { return "selected" }
func (h *mismatchScopeHarness) WithOperation(context.Context, provider.Ownership, string, func(context.Context, Harness) error) error {
	h.entered = true
	return errors.New("unexpected native scope")
}

func TestMismatchedHarnessReturnsToCoreWithoutAcquiringScope(t *testing.T) {
	harness := &mismatchScopeHarness{}
	operation := AgentRunOperation{messageIntent: core.MessageFollow, externals: Externals{
		Agent: harness,
		Ownership: func(context.Context, string) (provider.Ownership, error) {
			t.Fatal("mismatched Harness acquired ownership")
			return provider.Ownership{}, nil
		},
	}}
	run := core.AgentRun{Harness: "retained-other", State: core.AgentRunPending, ThreadID: "thread"}
	rejection := errors.New("Core rejected conflicting Harness")
	called := false
	err := operation.WithScope(context.Background(), run, func(_ context.Context, bound core.AgentRunOperation) error {
		called = true
		if bound.Harness() != "selected" {
			t.Fatal("scope hid conflicting Harness from Core")
		}
		return rejection
	})
	if !called || !errors.Is(err, rejection) || harness.entered {
		t.Fatalf("callback=%v native scope=%v err=%v", called, harness.entered, err)
	}
}
