package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
)

func (s Store) AdmitDirect(ctx context.Context, input core.SessionAdmission, queueName string) (core.Session, bool, error) {
	input.Workflow = core.WorkflowName(strings.TrimSpace(string(input.Workflow)))
	input.WorkflowRevision = strings.TrimSpace(input.WorkflowRevision)
	if input.Workflow != "" || input.WorkflowRevision != "" {
		return core.Session{}, false, fmt.Errorf("direct admission cannot use workflow identity")
	}
	normalized, err := normalizeCoreAdmission(input)
	if err != nil {
		return core.Session{}, false, err
	}
	session, created, err := admitSession(ctx, s, normalized, queueName, direct.TaskName, direct.TaskKey(core.SessionID(normalized.AdmissionKey)))
	if errors.Is(err, ErrAdmissionConflict) {
		err = fmt.Errorf("%w: %w", direct.ErrAdmissionConflict, err)
	}
	return session, created, err
}

func (s Store) AdmitDirectMessage(ctx context.Context, input core.MessageAdmission) (core.MessageAdmissionResult, error) {
	return s.admitMessage(ctx, input)
}

// resolveDirectMessageEnvelope supplies only the direct execution envelope. Generic
// Message admission owns Follow, Steer, ordering, and Thread binding.
func resolveDirectMessageEnvelope(input core.MessageAdmission) (admittedAgentRun, error) {
	if input.SandboxID != core.MainSandboxName(input.SessionID) {
		return admittedAgentRun{}, fmt.Errorf("direct Message requires the exact default Sandbox")
	}
	return admittedAgentRun{Role: direct.DirectAgentRole, SandboxID: input.SandboxID}, nil
}
