package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const (
	inspectFollowPollInterval  = time.Second
	inspectFollowPulseInterval = time.Minute
	followHistoryRow           = 12
	followLiveStatusRows       = 5
)

type followSnapshot struct {
	Job             spine.Job
	Profile         spine.SandboxProfile
	Definition      workflow.Definition
	History         []historyEntry
	Operation       string
	OperationDetail string
	NeedsAttention  bool
	AgentRuns       []spine.AgentRun
	Sandboxes       []spine.Sandbox
	Actions         []spine.Action
	Execution       taskResultView
}

func followJob(ctx context.Context, store postgres.Store, client *absurd.Client, records evidence.Store, jobID string, output io.Writer) error {
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

func loadFollowSnapshot(ctx context.Context, store postgres.Store, client *absurd.Client, records evidence.Store, jobID string) (followSnapshot, error) {
	job, err := store.Job(ctx, jobID)
	if err != nil {
		return followSnapshot{}, err
	}
	profile, err := store.SandboxProfile(ctx, job.SandboxProfile)
	if err != nil {
		return followSnapshot{}, err
	}
	execution, err := fetchTaskResult(ctx, client, job.TaskID)
	if err != nil {
		return followSnapshot{}, err
	}
	switch job.Workflow {
	case spine.WorkflowCodingToProposal:
		snapshot, err := workflow.LoadSnapshot(ctx, store, jobID)
		if err != nil {
			return followSnapshot{}, err
		}
		projection, err := snapshot.Project(records)
		if err != nil {
			return followSnapshot{}, err
		}
		runs := make([]spine.AgentRun, 0, len(snapshot.Deliveries))
		for _, delivery := range snapshot.Deliveries {
			runs = append(runs, delivery.AgentRun)
		}
		return followSnapshot{
			Job: snapshot.Job, Profile: profile, Definition: workflow.CodingToProposalDefinition(), History: workflowHistory(snapshot), Operation: projection.CurrentWork.Description(),
			OperationDetail: projection.CurrentWork.Detail, NeedsAttention: projection.CurrentWork.Kind == workflow.WorkAttention,
			AgentRuns: runs, Sandboxes: snapshot.Sandboxes, Actions: snapshot.Actions, Execution: execution,
		}, nil
	case spine.WorkflowCodebaseInvestigation:
		snapshot, err := workflow.LoadCodebaseInvestigation(ctx, store, jobID)
		if err != nil {
			return followSnapshot{}, err
		}
		work := snapshot.Project()
		return followSnapshot{
			Job: snapshot.Job, Profile: profile, Definition: workflow.CodebaseInvestigationDefinition(), History: investigationHistory(snapshot), Operation: work.Description(), OperationDetail: work.Detail,
			NeedsAttention: work.Kind == workflow.InvestigationWorkAttention, AgentRuns: []spine.AgentRun{snapshot.Delivery.AgentRun},
			Sandboxes: []spine.Sandbox{snapshot.MainSandbox}, Actions: snapshot.Actions, Execution: execution,
		}, nil
	default:
		return followSnapshot{}, fmt.Errorf("inspect --follow does not support workflow %q", job.Workflow)
	}
}

func (s followSnapshot) followTerminal() bool {
	return s.Job.CleanupState == spine.CleanupComplete || s.NeedsAttention || s.Job.CleanupAttention != "" || (s.Execution.State == absurd.TaskFailed && s.Job.AdmissionOpen)
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
		fmt.Fprintf(r.output, "Following Job %s (%s revision %s); Ctrl-C to stop\n", snapshot.Job.ID, snapshot.Job.Workflow, snapshot.Job.WorkflowRevision)
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
	if snapshot.Job.CleanupState == spine.CleanupComplete {
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
	case snapshot.Execution.State == absurd.TaskFailed && snapshot.Job.AdmissionOpen:
		summary = "Workflow stopped"
	case snapshot.NeedsAttention && snapshot.Job.WorkflowAttention != "":
		summary = "Needs attention · " + snapshot.Job.WorkflowAttention
	case snapshot.NeedsAttention:
		summary = "Workflow needs attention"
	case snapshot.Job.CleanupAttention != "":
		summary = "Cleanup needs attention · " + snapshot.Job.CleanupAttention
	}
	if summary == "" || summary == r.lastAttention {
		return
	}
	r.lastAttention = summary
	renderHumanHistoryEntry(r.output, historyEntry{At: observedAt, Text: summary}, &r.lastHistoryDate, "")
	if snapshot.Execution.State == absurd.TaskFailed && snapshot.Job.AdmissionOpen {
		if snapshot.Execution.LastError != "" {
			fmt.Fprintf(r.output, "         reason: %s\n", snapshot.Execution.LastError)
		}
		fmt.Fprintf(r.output, "         next: repair the cause, then run dorf retry %s\n", snapshot.Job.ID)
	}
}

func (r *followRenderer) renderPulse(observedAt time.Time, snapshot followSnapshot) {
	for _, run := range snapshot.AgentRuns {
		if run.State != spine.AgentRunActive {
			continue
		}
		detail := fmt.Sprintf("%s active", agentRunHumanRole(snapshot.Definition, run))
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
	title := fmt.Sprintf("Following Job %s · %s revision %s", snapshot.Job.ID, snapshot.Job.Workflow, snapshot.Job.WorkflowRevision)
	instruction := "Ctrl-C stops following; it does not stop the Job."
	if snapshot.Job.CleanupState == spine.CleanupComplete {
		heading = "Complete"
		title = fmt.Sprintf("Job %s · %s revision %s", snapshot.Job.ID, snapshot.Job.Workflow, snapshot.Job.WorkflowRevision)
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

func liveFollowStatuses(observedAt time.Time, snapshot followSnapshot) []string {
	if snapshot.Job.CleanupState == spine.CleanupComplete {
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
		if run.State != spine.AgentRunActive {
			continue
		}
		detail := agentRunHumanRole(snapshot.Definition, run) + " · active"
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

func sandboxRuntimeLabel(profile spine.SandboxProfile, selected string) string {
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

func provisionedSandboxes(job spine.Job, runs []spine.AgentRun, sandboxes []spine.Sandbox, actions []spine.Action) []provisionedSandbox {
	active := make([]provisionedSandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		createdAt, created := settledActionAt(actions, spine.ActionSandboxCreate, sandbox.ID)
		_, deleted := settledActionAt(actions, spine.ActionSandboxDelete, sandbox.ID)
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
