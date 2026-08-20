package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/investigation"
)

type workflowPresentation interface {
	OperationLabel(string, string) string
	ActionLabel(core.ActionKind) string
	AgentRoleLabel(string) string
	ResultLabel(string) string
}

// historyEntry is disposable human copy projected from durable product facts.
// It is neither persisted nor exposed by inspect --json.
type historyEntry struct {
	At   time.Time
	Text string
}

func workflowHistory(snapshot coding.Snapshot) []historyEntry {
	definition := coding.WorkflowDefinition()
	entries := commonHistory(snapshot.Job.Job, snapshot.Deliveries, snapshot.Actions)
	add := func(at time.Time, text string) { addHistoryEntry(&entries, at, text) }

	for _, delivery := range snapshot.Deliveries {
		message := delivery.Message
		add(message.AdmittedAt, fmt.Sprintf("Message %d received from %s", message.Sequence, humanIdentifier(string(message.FromKind))))
	}
	for _, delivery := range snapshot.Deliveries {
		addAgentRunHistory(&entries, definition, delivery)
	}
	for _, revision := range snapshot.Revisions {
		text := fmt.Sprintf("Revision generation %d observed · %s", revision.Generation, revision.OID)
		if revision.Generation == 0 {
			text = "Starting Revision accepted · " + revision.OID
		} else if revision.ComparisonBase != "" {
			text += " · from " + revision.ComparisonBase
		}
		add(revision.ObservedAt, text)
	}
	for _, plan := range snapshot.ReviewPlans {
		text := fmt.Sprintf("Review %s · roles %v", plan.Plan.Decision, plan.Plan.Roles)
		add(plan.RecordedAt, text)
	}
	for _, record := range snapshot.Evidence {
		text := humanIdentifier(record.Kind) + " evidence recorded"
		if record.Revision != "" {
			text += " · Revision " + record.Revision
		}
		add(record.FinishedAt, text)
	}
	if snapshot.Proposal != nil {
		add(proposalActionSettledAt(snapshot), fmt.Sprintf("Pull request #%d ready · Revision %s", snapshot.Proposal.Number, snapshot.Proposal.ProposedRevision))
	}
	if snapshot.Outcome != nil {
		text := "Outcome " + humanIdentifier(string(snapshot.Outcome.Kind))
		if snapshot.Outcome.ObservedState != "" {
			text += " · GitHub " + snapshot.Outcome.ObservedState
		}
		add(snapshot.Outcome.ObservedAt, text)
	}
	return sortedHistory(entries)
}

func investigationHistory(snapshot investigation.Snapshot) []historyEntry {
	definition := investigation.WorkflowDefinition()
	entries := commonHistory(snapshot.Job, snapshot.Deliveries, snapshot.Actions)
	for _, delivery := range snapshot.Deliveries {
		if delivery.Message.Sequence > 1 {
			addHistoryEntry(&entries, delivery.Message.AdmittedAt, fmt.Sprintf("Follow-up Message %d received", delivery.Message.Sequence))
		}
		addAgentRunHistory(&entries, definition, delivery)
	}
	for _, draft := range snapshot.Drafts {
		addHistoryEntry(&entries, draft.CreatedAt, definition.ResultLabel("investigation-draft")+" ready · "+draft.ArtifactID)
	}
	return sortedHistory(entries)
}

func commonHistory(job core.Job, deliveries []core.Delivery, actions []core.Action) []historyEntry {
	entries := make([]historyEntry, 0, 3+2*len(actions))
	addHistoryEntry(&entries, job.AdmittedAt, "Job admitted")
	if strings.TrimSpace(job.WorkflowAttention) != "" {
		addHistoryEntry(&entries, job.WorkflowAttentionAt, "Needs attention · "+job.WorkflowAttention)
	}
	runs := make([]core.AgentRun, 0, len(deliveries))
	for _, delivery := range deliveries {
		runs = append(runs, delivery.AgentRun)
	}
	for _, action := range actions {
		addHistoryEntry(&entries, action.CreatedAt, actionStartedText(job, runs, action))
		if !action.SettledAt.IsZero() {
			addHistoryEntry(&entries, action.SettledAt, actionSettledText(job, runs, action, actions))
		}
	}
	addHistoryEntry(&entries, job.CleanedAt, "Cleanup complete")
	return entries
}

func addAgentRunHistory(entries *[]historyEntry, definition workflowPresentation, delivery core.Delivery) {
	run := delivery.AgentRun
	role := agentRunHumanRole(definition, run)
	context := ""
	if delivery.Message.Sequence > 1 {
		context = fmt.Sprintf(" · Message %d", delivery.Message.Sequence)
	}
	addHistoryEntry(entries, run.StartedAt, role+" started"+context)
	if run.FinishedAt.IsZero() {
		return
	}
	text := role + " " + string(run.State)
	if !run.StartedAt.IsZero() {
		text += " · " + formatElapsed(run.FinishedAt.Sub(run.StartedAt))
	}
	addHistoryEntry(entries, run.FinishedAt, text+context)
}

func agentRunHumanRole(definition workflowPresentation, run core.AgentRun) string {
	role := definition.AgentRoleLabel(run.Role)
	if run.Capability == coding.ReviewReadOnlyCapability && !strings.Contains(strings.ToLower(role), "reviewer") {
		role += " reviewer"
	}
	return role
}

func actionStartedText(job core.Job, runs []core.AgentRun, action core.Action) string {
	role := sandboxHumanRole(job, runs, action.Scope)
	sandbox := role + " Sandbox"
	switch action.Kind {
	case core.ActionSandboxCreate:
		return withHumanDetails("Creating "+sandbox, sandboxProviderName(job.SandboxProfile))
	case gitworkspace.ActionRepositoryClone:
		return "Cloning repository"
	case coding.ActionRepositoryPush:
		return "Publishing Revision"
	case coding.ActionGitHubPullRequest:
		return "Creating pull request"
	case coding.ActionReviewCheckout:
		return "Preparing reviewer checkout"
	case core.ActionRouteCreate:
		return "Connecting model access"
	case core.ActionRouteRevoke:
		return "Revoking model access"
	case core.ActionSandboxDelete:
		return "Deleting " + sandbox
	default:
		return humanIdentifier(string(action.Kind)) + " started"
	}
}

func actionSettledText(job core.Job, runs []core.AgentRun, action core.Action, actions []core.Action) string {
	role := sandboxHumanRole(job, runs, action.Scope)
	sandbox := titleFirst(role) + " Sandbox"
	if action.State == core.ActionFailed {
		return actionFailureSubject(action.Kind, sandbox) + " failed"
	}
	duration := actionDuration(action)
	switch action.Kind {
	case core.ActionSandboxCreate:
		return withHumanDetails(sandbox+" ready", sandboxProviderName(job.SandboxProfile), duration)
	case gitworkspace.ActionRepositoryClone:
		return withHumanDetails("Repository cloned", duration)
	case coding.ActionRepositoryPush:
		return withHumanDetails("Revision published", duration)
	case coding.ActionGitHubPullRequest:
		return withHumanDetails("Pull request created", duration)
	case coding.ActionReviewCheckout:
		return withHumanDetails("Reviewer checkout ready", duration)
	case core.ActionRouteCreate:
		return withHumanDetails("Model access connected", duration)
	case core.ActionRouteRevoke:
		return withHumanDetails("Model access revoked", duration)
	case core.ActionSandboxDelete:
		createdAt, created := settledActionAt(actions, core.ActionSandboxCreate, action.Scope)
		if created && !createdAt.IsZero() {
			return withHumanDetails(sandbox+" deleted", "provisioned "+formatElapsed(action.SettledAt.Sub(createdAt)))
		}
		return sandbox + " deleted"
	default:
		return withHumanDetails(humanIdentifier(string(action.Kind))+" completed", duration)
	}
}

func actionFailureSubject(kind core.ActionKind, sandbox string) string {
	switch kind {
	case core.ActionSandboxCreate:
		return sandbox + " creation"
	case gitworkspace.ActionRepositoryClone:
		return "Repository clone"
	case coding.ActionRepositoryPush:
		return "Revision publication"
	case coding.ActionGitHubPullRequest:
		return "Pull request creation"
	case coding.ActionReviewCheckout:
		return "Reviewer checkout"
	case core.ActionRouteCreate:
		return "Model access connection"
	case core.ActionRouteRevoke:
		return "Model access revocation"
	case core.ActionSandboxDelete:
		return sandbox + " deletion"
	default:
		return humanIdentifier(string(kind))
	}
}

func actionDuration(action core.Action) string {
	if action.CreatedAt.IsZero() || action.SettledAt.IsZero() {
		return ""
	}
	duration := action.SettledAt.Sub(action.CreatedAt)
	if duration < time.Second {
		return ""
	}
	return formatElapsed(duration)
}

func sandboxHumanRole(job core.Job, runs []core.AgentRun, sandboxID string) string {
	if sandboxID != "" && sandboxID == core.MainSandboxName(job.ID) {
		return "primary"
	}
	for _, run := range runs {
		if run.SandboxID == sandboxID && sandboxID == coding.ReviewSandboxName(run.ID) {
			return "reviewer"
		}
	}
	return "sandbox"
}

func settledActionAt(actions []core.Action, kind core.ActionKind, scope string) (time.Time, bool) {
	for _, action := range actions {
		if action.Kind == kind && action.Scope == scope && action.State == core.ActionSucceeded {
			return action.SettledAt, true
		}
	}
	return time.Time{}, false
}

func addHistoryEntry(entries *[]historyEntry, at time.Time, text string) {
	if at.IsZero() || strings.TrimSpace(text) == "" {
		return
	}
	*entries = append(*entries, historyEntry{At: at, Text: text})
}

func sortedHistory(entries []historyEntry) []historyEntry {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.Before(entries[j].At) })
	return entries
}

func withHumanDetails(text string, details ...string) string {
	for _, detail := range details {
		if detail = strings.TrimSpace(detail); detail != "" {
			text += " · " + detail
		}
	}
	return text
}

func humanIdentifier(value string) string {
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(strings.TrimSpace(value)))
	if len(words) == 0 {
		return "Unknown"
	}
	words[0] = titleFirst(words[0])
	return strings.Join(words, " ")
}

func titleFirst(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func renderHistory(output io.Writer, entries []historyEntry) {
	fmt.Fprintln(output, "\nHistory")
	lastDate := ""
	for _, entry := range entries {
		renderHumanHistoryEntry(output, entry, &lastDate, "  ")
	}
}

func renderHumanHistoryEntry(output io.Writer, entry historyEntry, lastDate *string, indent string) {
	local := entry.At.In(time.Local)
	date := local.Format("2 Jan 2006")
	if date != *lastDate {
		if *lastDate != "" {
			fmt.Fprintln(output)
		}
		fmt.Fprintf(output, "%s%s\n", indent, date)
		*lastDate = date
	}
	fmt.Fprintf(output, "%s  %s  %s\n", indent, local.Format("15:04"), entry.Text)
}

func proposalActionSettledAt(s coding.Snapshot) time.Time {
	if s.Proposal == nil {
		return time.Time{}
	}
	for _, action := range s.Actions {
		if action.Kind == coding.ActionGitHubPullRequest && action.State == core.ActionSucceeded && action.Scope == s.Proposal.ProposedRevision {
			return action.SettledAt
		}
	}
	return time.Time{}
}
