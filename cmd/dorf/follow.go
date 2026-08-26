package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	inspectFollowPollInterval  = time.Second
	inspectFollowPulseInterval = time.Minute
	followHistoryRow           = 12
	followLiveStatusRows       = 5
)

type followSnapshot struct {
	Job             core.Job
	Profile         core.SandboxProfile
	Presentation    jobPresentation
	History         []historyEntry
	Operation       string
	OperationDetail string
	NeedsAttention  bool
	AgentRuns       []core.AgentRun
	Sandboxes       []core.Sandbox
	Actions         []core.Action
	Execution       taskResultView
}

func followJob(ctx context.Context, store postgres.Store, client *absurd.Client, records blob.Store, jobID string, output io.Writer) error {
	load := func(loadCtx context.Context) (followSnapshot, error) {
		return loadFollowSnapshot(loadCtx, store, client, records, jobID)
	}
	snapshot, err := load(ctx)
	if err != nil {
		return err
	}
	renderer := newFollowRenderer(output)
	renderer.Start(snapshot)
	defer renderer.Close()
	now := time.Now()
	renderer.Render(now, snapshot, true)
	if snapshot.followTerminal() {
		return nil
	}

	ticker := time.NewTicker(inspectFollowPollInterval)
	defer ticker.Stop()
	nextPulse := now.Add(inspectFollowPulseInterval)
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return nil
			}
			return ctx.Err()
		case now = <-ticker.C:
			snapshot, err = load(ctx)
			if err != nil {
				return err
			}
			pulse := renderer.interactive || !now.Before(nextPulse)
			renderer.Render(now, snapshot, pulse)
			if pulse && !renderer.interactive {
				nextPulse = now.Add(inspectFollowPulseInterval)
			}
			if snapshot.followTerminal() {
				return nil
			}
		}
	}
}

func loadFollowSnapshot(ctx context.Context, store postgres.Store, client *absurd.Client, records blob.Store, jobID string) (followSnapshot, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return followSnapshot{}, err
	}
	profile, err := store.SandboxProfile(ctx, job.SandboxProfile)
	if err != nil {
		return followSnapshot{}, err
	}
	execution, err := fetchTaskResult(ctx, client, job.CurrentTaskID)
	if err != nil {
		return followSnapshot{}, err
	}
	switch job.Workflow {
	case "":
		if job.WorkflowRevision != "" {
			return followSnapshot{}, fmt.Errorf("client-directed Job %s has a workflow revision without a workflow", job.ID)
		}
		snapshot, err := direct.LoadSnapshot(ctx, store, job)
		if err != nil {
			return followSnapshot{}, err
		}
		operation, detail, attention, _ := directOperation(snapshot.Project())
		return followSnapshot{
			Job: job, Profile: profile, Presentation: coreJobPresentation{}, History: directJobHistory(job, snapshot.Deliveries, snapshot.Actions),
			Operation: operation, OperationDetail: detail, NeedsAttention: attention,
			AgentRuns: deliveryAgentRuns(snapshot.Deliveries), Sandboxes: snapshot.Sandboxes,
			Actions: snapshot.Actions, Execution: execution,
		}.withCleanupOperation(), nil
	case coding.Workflow:
		snapshot, err := coding.LoadSnapshot(ctx, store, jobID)
		if err != nil {
			return followSnapshot{}, err
		}
		projection, err := snapshot.Project(records)
		if err != nil {
			return followSnapshot{}, err
		}
		deliveries, err := store.Deliveries(ctx, jobID)
		if err != nil {
			return followSnapshot{}, err
		}
		return followSnapshot{
			Job: snapshot.Job.Job, Profile: profile, Presentation: coding.WorkflowDefinition(), History: workflowHistory(snapshot, deliveries), Operation: projection.CurrentWork.Description(),
			OperationDetail: projection.CurrentWork.Detail, NeedsAttention: projection.CurrentWork.Kind == coding.WorkAttention,
			AgentRuns: deliveryAgentRuns(deliveries), Sandboxes: snapshot.Sandboxes, Actions: snapshot.Actions, Execution: execution,
		}.withCleanupOperation(), nil
	case investigation.Workflow:
		snapshot, err := investigation.LoadSnapshot(ctx, store, jobID)
		if err != nil {
			return followSnapshot{}, err
		}
		deliveries, err := store.Deliveries(ctx, jobID)
		if err != nil {
			return followSnapshot{}, err
		}
		work := snapshot.Project()
		return followSnapshot{
			Job: snapshot.Job, Profile: profile, Presentation: investigation.WorkflowDefinition(), History: investigationHistory(snapshot, deliveries), Operation: work.Description(), OperationDetail: work.Detail,
			NeedsAttention: work.Kind == investigation.WorkAttention, AgentRuns: deliveryAgentRuns(deliveries),
			Sandboxes: []core.Sandbox{snapshot.MainSandbox}, Actions: snapshot.Actions, Execution: execution,
		}.withCleanupOperation(), nil
	default:
		return followSnapshot{}, fmt.Errorf("inspect --follow does not support workflow %q", job.Workflow)
	}
}

func (s followSnapshot) followTerminal() bool {
	return s.Job.CleanupState == core.CleanupComplete || s.NeedsAttention || s.executionFailed()
}

func (s followSnapshot) executionFailed() bool {
	return s.Execution.State == absurd.TaskFailed && s.Job.CleanupState != core.CleanupComplete
}

func (s followSnapshot) withCleanupOperation() followSnapshot {
	if operation, ok := cleanupOperation(s.Presentation, s.Job, s.Sandboxes, s.Actions); ok {
		s.Operation = operation
		s.OperationDetail = ""
	}
	return s
}

func cleanupOperation(definition jobPresentation, job core.Job, sandboxes []core.Sandbox, actions []core.Action) (string, bool) {
	if job.CleanupState != core.CleanupScheduled {
		return "", false
	}
	if kind, _, pending := core.CurrentCleanupAction(sandboxes, actions); pending {
		return definition.ActionLabel(kind), true
	}
	return "Finalizing cleanup", true
}

type followRenderer struct {
	output          io.Writer
	interactive     bool
	started         bool
	seenHistory     map[string]struct{}
	lastHistoryDate string
	lastOperation   string
	lastAttention   string
}

func newFollowRenderer(output io.Writer) *followRenderer {
	return &followRenderer{output: output, interactive: interactiveFollowOutput(output), seenHistory: map[string]struct{}{}}
}

func (r *followRenderer) Start(snapshot followSnapshot) {
	r.started = true
	if !r.interactive {
		fmt.Fprintf(r.output, "Following Job %s (%s); Ctrl-C to stop\n", snapshot.Job.ID, jobContractLabel(snapshot.Job))
		return
	}
	fmt.Fprint(r.output, "\x1b[2J\x1b[H")
	r.renderInteractiveHeader(time.Now(), snapshot)
	fmt.Fprintf(r.output, "\x1b[%d;r\x1b[%d;1H", followHistoryRow, followHistoryRow)
}

func (r *followRenderer) Close() {
	if r.interactive && r.started {
		fmt.Fprint(r.output, "\x1b[s\x1b[r\x1b[u")
	}
}

func (r *followRenderer) Render(observedAt time.Time, snapshot followSnapshot, pulse bool) {
	for _, entry := range snapshot.History {
		key := historyEntryKey(entry)
		if _, seen := r.seenHistory[key]; seen {
			continue
		}
		r.seenHistory[key] = struct{}{}
		renderHumanHistoryEntry(r.output, entry, &r.lastHistoryDate, "")
	}
	operation := strings.TrimSpace(snapshot.Operation)
	if snapshot.Job.CleanupState == core.CleanupComplete {
		operation = ""
	}
	if detail := strings.TrimSpace(snapshot.OperationDetail); operation != "" && detail != "" {
		operation += " — " + detail
	}
	if !r.interactive && operation != "" && operation != r.lastOperation {
		r.lastOperation = operation
		renderFollowStatusLine(r.output, observedAt, "Current", operation)
	}
	r.renderAttention(observedAt, snapshot)
	if r.interactive {
		r.renderInteractiveHeader(observedAt, snapshot)
	} else if pulse {
		r.renderPulse(observedAt, snapshot)
	}
}

func (r *followRenderer) renderAttention(observedAt time.Time, snapshot followSnapshot) {
	var summary string
	switch {
	case snapshot.executionFailed() && snapshot.Job.CleanupState == core.CleanupScheduled:
		summary = "Cleanup stopped"
	case snapshot.executionFailed():
		if snapshot.Job.Workflow == "" {
			summary = "Job stopped"
		} else {
			summary = "Workflow stopped"
		}
	case snapshot.NeedsAttention && snapshot.Job.WorkflowAttention != "":
		summary = "Needs attention · " + snapshot.Job.WorkflowAttention
	case snapshot.NeedsAttention:
		if snapshot.Job.Workflow == "" {
			summary = "Job needs attention"
		} else {
			summary = "Workflow needs attention"
		}
	}
	if summary == "" || summary == r.lastAttention {
		return
	}
	r.lastAttention = summary
	renderHumanHistoryEntry(r.output, historyEntry{At: observedAt, Text: summary}, &r.lastHistoryDate, "")
	if snapshot.executionFailed() {
		if snapshot.Execution.LastError != "" {
			fmt.Fprintf(r.output, "         reason: %s\n", snapshot.Execution.LastError)
		}
		fmt.Fprintf(r.output, "         next: repair the cause, then run dorf retry %s\n", snapshot.Job.ID)
	} else if next := jobAttentionNext(snapshot.Job, snapshot.Execution); next != "" {
		fmt.Fprintf(r.output, "         next: %s\n", next)
	}
}

func (r *followRenderer) renderPulse(observedAt time.Time, snapshot followSnapshot) {
	for _, run := range snapshot.AgentRuns {
		if run.State != core.AgentRunActive {
			continue
		}
		detail := fmt.Sprintf("%s active", agentRunHumanRole(snapshot.Presentation, run))
		if !run.StartedAt.IsZero() {
			detail += " " + formatElapsed(observedAt.Sub(run.StartedAt))
		}
		renderFollowStatusLine(r.output, observedAt, "Pulse", detail)
	}
	for _, sandbox := range provisionedSandboxes(snapshot.Job, snapshot.AgentRuns, snapshot.Sandboxes, snapshot.Actions) {
		renderFollowStatusLine(r.output, observedAt, "Pulse", fmt.Sprintf("Sandbox %s · %s · provisioned %s", sandbox.Label, sandboxProviderName(snapshot.Job.SandboxProfile), formatElapsed(observedAt.Sub(sandbox.Since))))
	}
}

func (r *followRenderer) renderInteractiveHeader(observedAt time.Time, snapshot followSnapshot) {
	statuses := liveFollowStatuses(observedAt, snapshot)
	heading := "Live · refreshed every 1s"
	title := fmt.Sprintf("Following Job %s · %s", snapshot.Job.ID, jobContractLabel(snapshot.Job))
	instruction := "Ctrl-C stops following; it does not stop the Job."
	if snapshot.Job.CleanupState == core.CleanupComplete {
		heading = "Complete"
		title = fmt.Sprintf("Job %s · %s", snapshot.Job.ID, jobContractLabel(snapshot.Job))
		instruction = ""
	}
	lines := []string{
		title,
		instruction,
		"",
		heading,
	}
	for i := 0; i < followLiveStatusRows; i++ {
		if i < len(statuses) {
			lines = append(lines, statuses[i])
		} else {
			lines = append(lines, "")
		}
	}
	lines = append(lines, strings.Repeat("─", 60), "History")

	fmt.Fprint(r.output, "\x1b7")
	for row, line := range lines {
		fmt.Fprintf(r.output, "\x1b[%d;1H\x1b[2K%s", row+1, line)
	}
	fmt.Fprintf(r.output, "\x1b[%d;r\x1b8", followHistoryRow)
}

func jobContractLabel(job core.Job) string {
	if job.Workflow == "" && job.WorkflowRevision == "" {
		return "client-directed"
	}
	return fmt.Sprintf("%s revision %s", job.Workflow, job.WorkflowRevision)
}

func liveFollowStatuses(observedAt time.Time, snapshot followSnapshot) []string {
	if snapshot.Job.CleanupState == core.CleanupComplete {
		statuses := make([]string, 0, 2)
		if total := jobElapsed(snapshot.Job.AdmittedAt, snapshot.Job.CleanedAt); total != "" {
			statuses = append(statuses, fmt.Sprintf("  %-12s total · %s", "Job", total))
		}
		return append(statuses, fmt.Sprintf("  %-12s complete", "Cleanup"))
	}
	current := strings.TrimSpace(snapshot.Operation)
	if current == "" {
		current = "Unknown"
	}
	statuses := []string{fmt.Sprintf("  %-12s %s", "Current", current)}
	if elapsed := jobElapsed(snapshot.Job.AdmittedAt, observedAt); elapsed != "" {
		statuses = append(statuses, fmt.Sprintf("  %-12s elapsed · %s", "Job", elapsed))
	}
	for _, run := range snapshot.AgentRuns {
		if run.State != core.AgentRunActive {
			continue
		}
		detail := agentRunHumanRole(snapshot.Presentation, run) + " · active"
		if !run.StartedAt.IsZero() {
			detail += " " + formatElapsed(observedAt.Sub(run.StartedAt))
		}
		statuses = append(statuses, fmt.Sprintf("  %-12s %s", "AgentRun", detail))
	}
	provider := sandboxRuntimeLabel(snapshot.Profile, snapshot.Job.SandboxProfile)
	for _, sandbox := range provisionedSandboxes(snapshot.Job, snapshot.AgentRuns, snapshot.Sandboxes, snapshot.Actions) {
		statuses = append(statuses, fmt.Sprintf("  %-12s %s · %s · provisioned %s", "Sandbox", sandbox.Label, provider, formatElapsed(observedAt.Sub(sandbox.Since))))
	}
	if len(statuses) > followLiveStatusRows {
		statuses = statuses[:followLiveStatusRows]
	}
	return statuses
}

func jobElapsed(startedAt, finishedAt time.Time) string {
	if startedAt.IsZero() || finishedAt.IsZero() {
		return ""
	}
	return formatElapsed(finishedAt.Sub(startedAt))
}

func sandboxProviderName(profile string) string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "e2b":
		return "E2B"
	case "incus":
		return "Incus"
	default:
		return profile
	}
}

func sandboxRuntimeLabel(profile core.SandboxProfile, selected string) string {
	if profile.Name == "" || profile.Name != selected || profile.Provider == "" {
		return sandboxProviderName(selected)
	}
	return profile.Name + " · " + sandboxProviderName(string(profile.Provider))
}

func interactiveFollowOutput(output io.Writer) bool {
	file, ok := output.(*os.File)
	if !ok || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type provisionedSandbox struct {
	Label string
	Since time.Time
}

func provisionedSandboxes(job core.Job, runs []core.AgentRun, sandboxes []core.Sandbox, actions []core.Action) []provisionedSandbox {
	active := make([]provisionedSandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		createdAt, created := settledActionAt(actions, core.ActionSandboxCreate, sandbox.ID)
		_, deleted := settledActionAt(actions, core.ActionSandboxDelete, sandbox.ID)
		if !created || deleted || createdAt.IsZero() {
			continue
		}
		label := sandboxHumanRole(job, runs, sandbox.ID)
		active = append(active, provisionedSandbox{Label: label, Since: createdAt})
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Label < active[j].Label })
	return active
}

func historyEntryKey(entry historyEntry) string {
	return entry.At.UTC().Format(time.RFC3339Nano) + "\x00" + entry.Text
}

func renderFollowStatusLine(output io.Writer, at time.Time, kind, detail string) {
	fmt.Fprintf(output, "%s  %-12s %s\n", at.In(time.Local).Format(time.RFC3339), kind, detail)
}

func formatElapsed(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return duration.Truncate(time.Second).String()
}
