package core

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
	JobID    string
	FromKind MessageFromKind
	FromID   string
	Input    string
	Intent   MessageDeliveryIntent
}
