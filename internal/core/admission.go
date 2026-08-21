package core

import "context"

// JobAdmission is the complete Core input shared by every workflow admission.
// Workflow packages extend it with their own typed input.
type JobAdmission struct {
	AdmissionKey       string
	Workflow           WorkflowName
	WorkflowRevision   string
	Goal               string
	SandboxProfile     string
	ProviderConnection string
	Model              string
	ReasoningEffort    string
}

// MessageAdmission is one client input admitted to a workflow-owned FIFO.
type MessageAdmission struct {
	JobID     string
	SandboxID string
	FromKind  MessageFromKind
	FromID    string
	Input     string
	Intent    MessageDeliveryIntent
}

// MessageAdmissionResult is the immutable durable admission acknowledged by a
// workflow policy transaction. SandboxID is repeated independently of Message
// because Sandbox ownership belongs to the atomically admitted AgentRun.
type MessageAdmissionResult struct {
	Message   Message
	SandboxID string
	Created   bool
}

// AgentMessageAdmission is the provider-neutral composition seam behind an
// Agent handle. The deployment selects a known module and delegates to its
// typed policy transaction; Core never switches on workflow identity.
type AgentMessageAdmission interface {
	AdmitAgentMessage(context.Context, MessageAdmission) (MessageAdmissionResult, error)
}
