package main

import (
	"context"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
)

func TestDirectClientPromptIsExactAndFailClosed(t *testing.T) {
	job := core.Job{ID: "job-direct"}
	message := core.Message{ID: "message-direct", JobID: job.ID, Input: "raw caller prompt\nwith exact spacing\n"}
	sandbox := core.Sandbox{ID: core.MainSandboxName(job.ID), JobID: job.ID, Name: core.DefaultSandbox}
	execution := core.AgentMessageExecution{
		Job: job, Message: message, Sandbox: sandbox,
		AgentRun: core.AgentRun{ID: core.AgentRunID(message.ID), JobID: job.ID, MessageID: message.ID, Role: direct.DirectAgentRole, SandboxID: sandbox.ID},
	}
	resolved := composedAgentExecution{}
	prompt, err := resolved.ResolveAgentPrompt(context.Background(), execution)
	if err != nil || prompt != execution.Message.Input {
		t.Fatalf("direct prompt=%q err=%v", prompt, err)
	}
	if _, err := resolved.ResolveAgentRunOperation(context.Background(), execution); err != nil {
		t.Fatalf("direct operation: %v", err)
	}

	for name, mutate := range map[string]func(*core.AgentMessageExecution){
		"workflow":          func(value *core.AgentMessageExecution) { value.Job.Workflow = "foreign" },
		"workflow revision": func(value *core.AgentMessageExecution) { value.Job.WorkflowRevision = "foreign" },
		"role":              func(value *core.AgentMessageExecution) { value.AgentRun.Role = "implement" },
		"capability":        func(value *core.AgentMessageExecution) { value.AgentRun.Capability = "foreign" },
		"revision":          func(value *core.AgentMessageExecution) { value.AgentRun.InputRevision = "foreign" },
		"run Sandbox":       func(value *core.AgentMessageExecution) { value.AgentRun.SandboxID = "foreign" },
		"Sandbox name":      func(value *core.AgentMessageExecution) { value.Sandbox.Name = "foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := execution
			mutate(&changed)
			if _, err := resolved.ResolveAgentPrompt(context.Background(), changed); err == nil {
				t.Fatal("changed direct Agent contract resolved a prompt")
			}
			if _, err := resolved.ResolveAgentRunOperation(context.Background(), changed); err == nil {
				t.Fatal("changed direct Agent contract resolved a Harness operation")
			}
		})
	}
}
