package core

import "context"

// JobAdmission is the complete Core input shared by workflow and direct-client
// admission. Workflow packages extend it with their own typed input; a direct
// client leaves both workflow identity fields empty.
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

// MessageAdmission is one client input admitted to its exact Agent lane.
type MessageAdmission struct {
	JobID     string
	SandboxID string
	FromKind  MessageFromKind
	FromID    string
	Input     string
	Intent    MessageDeliveryIntent
}

// MessageAdmissionResult is the immutable durable admission acknowledged by
// the typed execution-envelope transaction. SandboxID is repeated independently of Message
// because Sandbox ownership belongs to the atomically admitted AgentRun.
type MessageAdmissionResult struct {
	Message   Message
	SandboxID string
	Created   bool
}

// AgentMessageAdmission is the provider-neutral composition seam behind an
// Agent handle. The deployment selects a typed execution-envelope adapter;
// Follow and Steer semantics remain invariant beneath it.
type AgentMessageAdmission interface {
	AdmitAgentMessage(context.Context, MessageAdmission) (MessageAdmissionResult, error)
}
