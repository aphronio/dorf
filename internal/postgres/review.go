package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

func (s Store) MarkChecksVerified(ctx context.Context, jobID, revision string, verifiedEvidenceIDs []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRevision, phase string
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase from dorf.jobs where id=$1 for update`, jobID).Scan(&currentRevision, &phase); err != nil {
		return err
	}
	if currentRevision != revision || phase != "checking" {
		return fmt.Errorf("Revision %s Checks cannot be finalized from phase %s at Revision %s", revision, phase, currentRevision)
	}
	if err := verifyEvidenceSet(ctx, tx, jobID, revision, verifiedEvidenceIDs); err != nil {
		return err
	}
	if err := ensureInputsTerminalForWorkflowTx(ctx, tx, jobID); err != nil {
		return fmt.Errorf("automatic review planning blocked: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.review_plans(job_id,revision,state) values($1,$2,'pending')`, jobID, revision); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='review-planning',workflow_attention=null where id=$1 and revision=$2 and workflow_phase='checking'`, jobID, revision)); err != nil {
		return err
	}
	return tx.Commit()
}

func verifyEvidenceSet(ctx context.Context, tx *sql.Tx, jobID, revision string, verifiedEvidenceIDs []string) error {
	rows, err := tx.QueryContext(ctx, `select c.evidence_id from dorf.repository_commands r join dorf.checks c on c.job_id=r.job_id and c.name=r.name and c.command=r.command and c.revision=$2 where r.job_id=$1 and r.name in ('check','smoke') and c.state='passed' and c.exit_code=0 order by r.name`, jobID, revision)
	if err != nil {
		return err
	}
	var proving []string
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !id.Valid || id.String == "" {
			rows.Close()
			return fmt.Errorf("Revision %s has a passing Check without Evidence", revision)
		}
		proving = append(proving, id.String)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var declared int
	if err := tx.QueryRowContext(ctx, `select count(*) from dorf.repository_commands where job_id=$1 and name in ('check','smoke')`, jobID).Scan(&declared); err != nil {
		return err
	}
	sort.Strings(proving)
	verified := append([]string(nil), verifiedEvidenceIDs...)
	sort.Strings(verified)
	if declared == 0 || len(proving) != declared || !slices.Equal(proving, verified) {
		return fmt.Errorf("Revision %s is not review-admissible: verified Evidence does not exactly match %d declared Checks", revision, declared)
	}
	return nil
}

func (s Store) ReviewPlan(ctx context.Context, jobID, revision string) (spine.ReviewPlanRecord, error) {
	return reviewPlanRow(s.DB.QueryRowContext(ctx, `select job_id,revision,state,coalesce(facts,'{}'::jsonb),coalesce(plan,'{}'::jsonb),coalesce(policy_digest,''),created_at,finalized_at from dorf.review_plans where job_id=$1 and revision=$2`, jobID, revision))
}

func (s Store) ReviewPlans(ctx context.Context, jobID string) ([]spine.ReviewPlanRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `select revision from dorf.review_plans where job_id=$1 order by created_at`, jobID)
	if err != nil {
		return nil, err
	}
	var revisions []string
	for rows.Next() {
		var revision string
		if err := rows.Scan(&revision); err != nil {
			rows.Close()
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	plans := make([]spine.ReviewPlanRecord, 0, len(revisions))
	for _, revision := range revisions {
		plan, err := s.ReviewPlan(ctx, jobID, revision)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func reviewPlanTx(ctx context.Context, tx *sql.Tx, jobID, revision string) (spine.ReviewPlanRecord, error) {
	return reviewPlanRow(tx.QueryRowContext(ctx, `select job_id,revision,state,coalesce(facts,'{}'::jsonb),coalesce(plan,'{}'::jsonb),coalesce(policy_digest,''),created_at,finalized_at from dorf.review_plans where job_id=$1 and revision=$2 for update`, jobID, revision))
}

func reviewPlanRow(row *sql.Row) (spine.ReviewPlanRecord, error) {
	var record spine.ReviewPlanRecord
	var facts, plan []byte
	var finalized sql.NullTime
	err := row.Scan(&record.JobID, &record.Revision, &record.State, &facts, &plan, &record.PolicyDigest, &record.CreatedAt, &finalized)
	if err != nil {
		return record, err
	}
	if finalized.Valid {
		record.FinalizedAt = finalized.Time
	}
	if string(facts) != "{}" {
		if err := json.Unmarshal(facts, &record.Facts); err != nil {
			return record, err
		}
	}
	if string(plan) != "{}" {
		if err := json.Unmarshal(plan, &record.Plan); err != nil {
			return record, err
		}
	}
	return record, nil
}

func policyDigest(facts policy.ChangeFacts, plan policy.ReviewPlan) (string, error) {
	contents, err := json.Marshal(struct {
		Facts policy.ChangeFacts `json:"facts"`
		Plan  policy.ReviewPlan  `json:"plan"`
	}{facts, plan})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

func (s Store) RecordReviewPolicy(ctx context.Context, proposed spine.ReviewPlanRecord) error {
	digest, err := policyDigest(proposed.Facts, proposed.Plan)
	if err != nil {
		return err
	}
	if proposed.Facts.Revision != proposed.Revision {
		return fmt.Errorf("review policy does not match its immutable Revision or decision")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, err := reviewPlanTx(ctx, tx, proposed.JobID, proposed.Revision)
	if err != nil {
		return err
	}
	if stored.PolicyDigest != "" {
		if stored.PolicyDigest != digest {
			return fmt.Errorf("mandatory review policy result changed across retry")
		}
		return tx.Commit()
	}
	if stored.State != "pending" {
		return fmt.Errorf("review policy conflicts with its durable per-Revision state")
	}
	factsJSON, _ := json.Marshal(proposed.Facts)
	planJSON, _ := json.Marshal(proposed.Plan)
	phase := "reviewing"
	if proposed.Plan.Decision == "no-review" {
		phase = "ready"
	}
	if _, err := tx.ExecContext(ctx, `update dorf.review_plans set state='final',facts=$3::jsonb,plan=$4::jsonb,policy_digest=$5,finalized_at=clock_timestamp() where job_id=$1 and revision=$2 and policy_digest is null`, proposed.JobID, proposed.Revision, factsJSON, planJSON, digest); err != nil {
		return err
	}
	for _, role := range proposed.Plan.Roles {
		if _, err := createReviewRunTx(ctx, tx, proposed.JobID, proposed.Revision, string(role), proposed.Facts); err != nil {
			return err
		}
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase=$3,workflow_attention=null where id=$1 and revision=$2 and workflow_phase='review-planning'`, proposed.JobID, proposed.Revision, phase)); err != nil {
		return err
	}
	return tx.Commit()
}

func createReviewRunTx(ctx context.Context, tx *sql.Tx, jobID, revision, role string, facts policy.ChangeFacts) (string, error) {
	runID := spine.ReviewAgentRunID(jobID, revision, role)
	workspace := "/workspace/job"
	var declaredChecks []string
	rows, err := tx.QueryContext(ctx, `select name from dorf.repository_commands where job_id=$1 and name in ('check','smoke') order by name`, jobID)
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		declaredChecks = append(declaredChecks, name)
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	input := policy.RolePrompt(policy.Role(role), facts, declaredChecks)
	turnAction := spine.ScopedActionID(jobID, spine.ActionTurnStart, runID)
	sessionAction := spine.ScopedActionID(jobID, spine.ActionSessionStart, runID)
	sandboxCreateAction := spine.ScopedActionID(jobID, spine.ActionSandboxCreate, runID)
	routeCreateAction := spine.ScopedActionID(jobID, spine.ActionRouteCreate, runID)
	createAction := spine.ScopedActionID(jobID, spine.ActionReviewWorkspaceCreate, runID)
	routeRevokeAction := spine.ScopedActionID(jobID, spine.ActionRouteRevoke, runID)
	sandboxDeleteAction := spine.ScopedActionID(jobID, spine.ActionSandboxDelete, runID)
	for _, action := range []struct {
		id   string
		kind spine.ActionKind
	}{{turnAction, spine.ActionTurnStart}, {sessionAction, spine.ActionSessionStart}, {sandboxCreateAction, spine.ActionSandboxCreate}, {routeCreateAction, spine.ActionRouteCreate}, {createAction, spine.ActionReviewWorkspaceCreate}, {routeRevokeAction, spine.ActionRouteRevoke}, {sandboxDeleteAction, spine.ActionSandboxDelete}} {
		if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state,scope_key) values($1,$2,$3,'pending',$4) on conflict do nothing`, action.id, jobID, action.kind, runID); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,action_id,role,state,revision,capability,workspace,input_contract) values($1,$2,$3,$4,'pending',$5,$6,$7,$8) on conflict do nothing`, runID, jobID, turnAction, role, revision, spine.ReviewReadOnlyCapability, workspace, input); err != nil {
		return "", err
	}
	ownerNonce, err := reviewNonce()
	if err != nil {
		return "", err
	}
	submissionNonce, err := reviewNonce()
	if err != nil {
		return "", err
	}
	inputSum := sha256.Sum256([]byte(input))
	if _, err := tx.ExecContext(ctx, `insert into dorf.review_resources(run_id,job_id,revision,sandbox_name,ownership_nonce,submission_nonce,input_digest,sandbox_create_action_id,route_create_action_id,materialize_action_id,route_revoke_action_id,sandbox_delete_action_id) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) on conflict do nothing`, runID, jobID, revision, spine.ReviewSandboxName(runID), ownerNonce, submissionNonce, hex.EncodeToString(inputSum[:]), sandboxCreateAction, routeCreateAction, createAction, routeRevokeAction, sandboxDeleteAction); err != nil {
		return "", err
	}
	return runID, nil
}

func reviewNonce() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate review ownership nonce: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func (s Store) ReviewRun(ctx context.Context, runID string) (spine.ReviewRunView, error) {
	return scanReviewRun(s.DB.QueryRowContext(ctx, reviewRunSelect+` where ar.id=$1`, runID))
}

const reviewRunSelect = `select ar.id,ar.job_id,coalesce(ar.message_id,''),ar.action_id,coalesce(ar.session_id,''),ar.state,ar.baseline_native_turn_id is not null,coalesce(ar.baseline_native_turn_id,''),coalesce(ar.native_turn_id,''),coalesce(ar.native_outcome,''),coalesce(ar.attention,''),ar.role,coalesce(ar.revision,''),coalesce(ar.capability,''),coalesce(ar.workspace,''),coalesce(ar.input_contract,''),coalesce(ar.claim_evidence_id,''),coalesce(ar.observed_evidence_id,''),ar.started_at,ar.finished_at,ar.input_tokens,ar.cached_input_tokens,ar.output_tokens,ar.cost_microusd,ar.usage_available,ar.yield_count,coalesce(rr.sandbox_name,''),coalesce(rr.route_id,''),coalesce(rr.app_server_id,''),coalesce(rr.ownership_nonce,''),coalesce(rr.submission_nonce,''),coalesce(rr.input_digest,''),coalesce(rr.revision_tree,''),coalesce(rr.sandbox_state,''),coalesce(rr.route_state,''),coalesce(rr.checkout_state,''),coalesce(rr.post_review_state,'') from dorf.agent_runs ar left join dorf.review_resources rr on rr.run_id=ar.id`

func scanReviewRun(row rowScanner) (spine.ReviewRunView, error) {
	var view spine.ReviewRunView
	run := &view.AgentRun
	projection := &view.ReviewRunProjection
	var started, finished sql.NullTime
	err := row.Scan(&run.ID, &run.JobID, &run.MessageID, &run.ActionID, &run.SessionID, &run.State, &run.BaselineRecorded, &run.BaselineTurnID, &run.NativeTurnID, &run.NativeOutcome, &run.Attention, &run.Role, &run.Revision, &run.Capability, &run.Workspace, &run.InputContract, &projection.ClaimEvidenceID, &projection.ObservedEvidenceID, &started, &finished, &run.InputTokens, &run.CachedInputTokens, &run.OutputTokens, &run.CostMicrousd, &run.UsageAvailable, &run.YieldCount, &projection.ReviewerSandboxID, &projection.ReviewerRouteID, &projection.ReviewerAppServer, &projection.ReviewerOwnerNonce, &projection.SubmissionNonce, &projection.InputDigest, &projection.RevisionTree, &projection.ReviewerSandboxState, &projection.ReviewerRouteState, &projection.CheckoutState, &projection.PostReviewState)
	if started.Valid {
		run.StartedAt = started.Time
	}
	if finished.Valid {
		run.FinishedAt = finished.Time
	}
	return view, err
}

func (s Store) ReviewRuns(ctx context.Context, jobID, revision string) ([]spine.ReviewRunView, error) {
	var currentRevision string
	if err := s.DB.QueryRowContext(ctx, `select revision from dorf.jobs where id=$1`, jobID).Scan(&currentRevision); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, reviewRunSelect+` where ar.job_id=$1 and ar.revision=$2 order by ar.role`, jobID, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []spine.ReviewRunView
	for rows.Next() {
		run, err := scanReviewRun(rows)
		if err != nil {
			return nil, err
		}
		view := run
		view.Stale = view.Revision != currentRevision
		err = s.DB.QueryRowContext(ctx, `select coalesce((select id from dorf.job_messages where job_id=$1 and from_kind='agent' and from_id=$2),'')`, run.JobID, run.ID).Scan(&view.FeedbackMessageID)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (s Store) AllReviewRuns(ctx context.Context, jobID string) ([]spine.ReviewRunView, error) {
	rows, err := s.DB.QueryContext(ctx, `select distinct revision from dorf.agent_runs where job_id=$1 and revision is not null order by revision`, jobID)
	if err != nil {
		return nil, err
	}
	var revisions []string
	for rows.Next() {
		var revision string
		if err := rows.Scan(&revision); err != nil {
			rows.Close()
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var result []spine.ReviewRunView
	for _, revision := range revisions {
		runs, err := s.ReviewRuns(ctx, jobID, revision)
		if err != nil {
			return nil, err
		}
		result = append(result, runs...)
	}
	return result, nil
}

// CleanupReviewRuns returns only AgentRuns backed by persisted reviewer
// resources, including partial rows that cleanup must validate or reject.
func (s Store) CleanupReviewRuns(ctx context.Context, jobID string) ([]spine.ReviewRunView, error) {
	rows, err := s.DB.QueryContext(ctx, reviewRunSelect+` where rr.job_id=$1 order by rr.revision,ar.role`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []spine.ReviewRunView
	for rows.Next() {
		run, err := scanReviewRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func (s Store) beginReviewAction(ctx context.Context, runID string, kind spine.ActionKind) (spine.Action, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Action{}, err
	}
	defer tx.Rollback()
	var actionID string
	switch kind {
	case spine.ActionSandboxCreate:
		err = tx.QueryRowContext(ctx, `select sandbox_create_action_id from dorf.review_resources where run_id=$1`, runID).Scan(&actionID)
	case spine.ActionRouteCreate:
		err = tx.QueryRowContext(ctx, `select route_create_action_id from dorf.review_resources where run_id=$1`, runID).Scan(&actionID)
	case spine.ActionReviewWorkspaceCreate:
		err = tx.QueryRowContext(ctx, `select materialize_action_id from dorf.review_resources where run_id=$1`, runID).Scan(&actionID)
	case spine.ActionSessionStart:
		actionID = spine.ScopedActionID("", kind, runID)
		err = tx.QueryRowContext(ctx, `select id from dorf.actions where kind=$2 and scope_key=$1`, runID, kind).Scan(&actionID)
	case spine.ActionRouteRevoke:
		err = tx.QueryRowContext(ctx, `select route_revoke_action_id from dorf.review_resources where run_id=$1`, runID).Scan(&actionID)
	case spine.ActionSandboxDelete:
		err = tx.QueryRowContext(ctx, `select sandbox_delete_action_id from dorf.review_resources where run_id=$1`, runID).Scan(&actionID)
	default:
		err = fmt.Errorf("unsupported review Action %q", kind)
	}
	if err != nil {
		return spine.Action{}, err
	}
	var action spine.Action
	err = tx.QueryRowContext(ctx, `select id,job_id,coalesce(message_id,''),kind,state,coalesce(external_id,''),coalesce(external_outcome,''),scope_key from dorf.actions where id=$1 for update`, actionID).Scan(&action.ID, &action.JobID, &action.MessageID, &action.Kind, &action.State, &action.ExternalID, &action.Outcome, &action.Scope)
	if err != nil {
		return spine.Action{}, err
	}
	if err = tx.Commit(); err != nil {
		return spine.Action{}, err
	}
	return action, nil
}

func (s Store) BeginReviewSandbox(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionSandboxCreate)
}
func (s Store) BeginReviewRoute(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionRouteCreate)
}
func (s Store) BeginReviewWorkspace(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionReviewWorkspaceCreate)
}
func (s Store) BeginReviewSession(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionSessionStart)
}

func (s Store) UncertainReviewSubmission(ctx context.Context, runID, sessionActionID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("review submission uncertainty requires a reason")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var turnActionID string
	if err := tx.QueryRowContext(ctx, `select ar.action_id from dorf.agent_runs ar join dorf.review_resources rr on rr.run_id=ar.id where ar.id=$1 and ar.state in ('submitting','uncertain') and ar.capability=$2 for update of ar`, runID, spine.ReviewReadOnlyCapability).Scan(&turnActionID); err != nil {
		return err
	}
	outcome := spine.ReviewSubmissionUncertainOutcome + ": " + reason
	if err := expectOne(tx.ExecContext(ctx, `update dorf.actions set state='uncertain',external_outcome=$3 where id=$1 and scope_key=$2 and kind='codex-session-start' and state in ('pending','uncertain')`, sessionActionID, runID, outcome)); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.actions set state='uncertain',external_outcome=$2 where id=$1 and state in ('pending','uncertain')`, turnActionID, outcome)); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.agent_runs set state='uncertain',attention=$2,updated_at=clock_timestamp() where id=$1 and session_id is null and native_turn_id is null`, runID, reason)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s Store) BeginReviewRouteCleanup(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionRouteRevoke)
}
func (s Store) BeginReviewSandboxCleanup(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionSandboxDelete)
}

func (s Store) InterruptReviewRun(ctx context.Context, runID, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `select ar.state from dorf.agent_runs ar join dorf.review_resources rr on rr.run_id=ar.id where ar.id=$1 and ar.capability=$2 for update of ar`, runID, spine.ReviewReadOnlyCapability).Scan(&state); err != nil {
		return err
	}
	if state == string(spine.AgentRunCompleted) || state == string(spine.AgentRunFailed) || state == string(spine.AgentRunInterrupted) {
		return tx.Commit()
	}
	var actionID string
	if err := tx.QueryRowContext(ctx, `update dorf.agent_runs set state='interrupted',native_outcome=case when native_turn_id is null then null else 'interrupted' end,attention=$2,finished_at=case when started_at is null then null else coalesce(finished_at,clock_timestamp()) end,updated_at=clock_timestamp() where id=$1 and state in ('pending','submitting','active','uncertain') returning action_id`, runID, reason).Scan(&actionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.actions set state='failed',external_outcome=$2 where id=$1 and state in ('pending','uncertain')`, actionID, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) RecordReviewPostState(ctx context.Context, runID string, receipt spine.Receipt) error {
	revision, tree, err := parseReviewStateOutcome(receipt.Outcome)
	if err != nil {
		return err
	}
	return expectOne(s.DB.ExecContext(ctx, `update dorf.review_resources rr set post_review_state='verified',post_review_verified_at=coalesce(post_review_verified_at,clock_timestamp()) from dorf.agent_runs ar where rr.run_id=$1 and ar.id=rr.run_id and rr.sandbox_state='created' and rr.route_state='active' and rr.checkout_state='verified' and rr.revision=$2 and rr.revision_tree=$3 and ar.workspace=$4`, runID, revision, tree, receipt.ExternalID))
}

func parseReviewStateOutcome(outcome string) (string, string, error) {
	parts := strings.Fields(outcome)
	if len(parts) != 3 || parts[2] != "clean" || !ValidRevision(parts[0]) || !ValidRevision(parts[1]) {
		return "", "", fmt.Errorf("review checkout observation is not exact Revision/tree/clean state")
	}
	return parts[0], parts[1], nil
}

// RecordReviewFeedback retains one reviewer's exact prose and feeds it back to
// the original implementation Session as an ordinary Message. The Job row is
// locked before allocating the Message sequence so concurrent reviewer
// completions remain deterministic and idempotent.
func (s Store) RecordReviewFeedback(ctx context.Context, runID string, outcome spine.NativeTurn, claim, observed spine.Evidence) (spine.Message, bool, error) {
	if outcome.Status != "completed" || strings.TrimSpace(outcome.Output) == "" {
		return spine.Message{}, false, fmt.Errorf("review feedback requires exact nonblank completed reviewer output")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Message{}, false, err
	}
	defer tx.Rollback()
	var jobID, revision, role, runState, nativeTurnID, capability, currentRevision, phase string
	if err := tx.QueryRowContext(ctx, `
		select ar.job_id,ar.revision,ar.role,ar.state,coalesce(ar.native_turn_id,''),coalesce(ar.capability,''),j.revision,j.workflow_phase
		from dorf.agent_runs ar join dorf.jobs j on j.id=ar.job_id
		where ar.id=$1 for update of j,ar`, runID).Scan(&jobID, &revision, &role, &runState, &nativeTurnID, &capability, &currentRevision, &phase); err != nil {
		return spine.Message{}, false, err
	}
	if !policy.Allowed(policy.Role(role)) || capability != spine.ReviewReadOnlyCapability || runState != string(spine.AgentRunCompleted) ||
		nativeTurnID == "" || outcome.ID != nativeTurnID || revision != currentRevision || claim.Revision != revision || observed.Revision != revision ||
		phase != "reviewing" && phase != "review-feedback" && phase != "ready" {
		return spine.Message{}, false, fmt.Errorf("review feedback conflicts with its completed exact-Revision reviewer AgentRun")
	}
	var boundaryReady bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from dorf.review_resources where run_id=$1 and sandbox_state='created' and route_state='active' and checkout_state='verified' and post_review_state='verified' and app_server_id is not null and revision_tree is not null)`, runID).Scan(&boundaryReady); err != nil {
		return spine.Message{}, false, err
	}
	if !boundaryReady {
		return spine.Message{}, false, fmt.Errorf("review feedback lacks an attested isolated reviewer boundary")
	}
	if err := insertEvidence(ctx, tx, jobID, claim); err != nil {
		return spine.Message{}, false, err
	}
	if err := insertEvidence(ctx, tx, jobID, observed); err != nil {
		return spine.Message{}, false, err
	}
	var storedClaimID, storedObservedID string
	if err := tx.QueryRowContext(ctx, `select coalesce(claim_evidence_id,''),coalesce(observed_evidence_id,'') from dorf.agent_runs where id=$1`, runID).Scan(&storedClaimID, &storedObservedID); err != nil {
		return spine.Message{}, false, err
	}
	if storedClaimID != "" && storedClaimID != claim.ID || storedObservedID != "" && storedObservedID != observed.ID {
		return spine.Message{}, false, fmt.Errorf("review feedback retry conflicts with retained Evidence")
	}
	if _, err := tx.ExecContext(ctx, `update dorf.agent_runs set claim_evidence_id=$2,observed_evidence_id=$3,input_tokens=$4,cached_input_tokens=$5,output_tokens=$6,cost_microusd=$7,usage_available=$8,finished_at=coalesce(finished_at,clock_timestamp()) where id=$1`, runID, claim.ID, observed.ID, outcome.InputTokens, outcome.CachedInputTokens, outcome.OutputTokens, outcome.CostMicrousd, outcome.UsageAvailable); err != nil {
		return spine.Message{}, false, err
	}

	message := spine.Message{ID: spine.MessageID(jobID, spine.MessageFromAgent, runID), JobID: jobID, FromKind: spine.MessageFromAgent, FromID: runID, Input: outcome.Output, Intent: spine.MessageFollow}
	created := false
	err = tx.QueryRowContext(ctx, `select id,job_id,from_kind,from_id,sequence,input,delivery_intent,coalesce(steer_target_turn_id,'') from dorf.job_messages where job_id=$1 and from_kind='agent' and from_id=$2`, jobID, runID).Scan(&message.ID, &message.JobID, &message.FromKind, &message.FromID, &message.Sequence, &message.Input, &message.Intent, &message.TargetTurnID)
	if err == nil {
		if message.Input != outcome.Output || message.Intent != spine.MessageFollow {
			return spine.Message{}, false, fmt.Errorf("reviewer AgentRun %s is already bound to different exact feedback", runID)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return spine.Message{}, false, err
	} else {
		message.Sequence, err = allocateMessageSequenceTx(ctx, tx, jobID)
		if err != nil {
			return spine.Message{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input) values($1,$2,$3,$4,$5,$6)`, message.ID, message.JobID, message.FromKind, message.FromID, message.Sequence, message.Input); err != nil {
			return spine.Message{}, false, err
		}
		actionID, implementationRunID := spine.TurnActionID(message.ID), spine.AgentRunID(message.ID)
		if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,message_id,kind,state) values($1,$2,$3,$4,'pending')`, actionID, jobID, message.ID, spine.ActionTurnStart); err != nil {
			return spine.Message{}, false, err
		}
		if err := expectOne(tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,message_id,action_id,session_id,role,state) select $1,$2,$3,$4,native_session_id,'implement','pending' from dorf.sessions where job_id=$2`, implementationRunID, jobID, message.ID, actionID)); err != nil {
			return spine.Message{}, false, err
		}
		created = true
	}
	var missing int
	if err := tx.QueryRowContext(ctx, `select count(*) from dorf.review_resources rr where rr.job_id=$1 and rr.revision=$2 and not exists(select 1 from dorf.job_messages m where m.job_id=rr.job_id and m.from_kind='agent' and m.from_id=rr.run_id)`, jobID, revision).Scan(&missing); err != nil {
		return spine.Message{}, false, err
	}
	if missing == 0 && phase == "reviewing" {
		if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='review-feedback',workflow_attention=null where id=$1 and revision=$2 and workflow_phase='reviewing'`, jobID, revision)); err != nil {
			return spine.Message{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return spine.Message{}, false, err
	}
	return message, created, nil
}

// CompleteReviewFeedback accepts an unchanged clean checkout only while the
// named implementation AgentRun is still the latest accepted Message. A later
// Message wins the Job-row race and returns false so it can be delivered.
func (s Store) CompleteReviewFeedback(ctx context.Context, jobID, runID, revision string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var currentRevision, phase string
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase from dorf.jobs where id=$1 for update`, jobID).Scan(&currentRevision, &phase); err != nil {
		return false, err
	}
	if currentRevision != revision || phase != "review-feedback" {
		return false, fmt.Errorf("unchanged review feedback conflicts with current Revision %s or workflow phase %s", currentRevision, phase)
	}
	candidate, ready, err := revisionCandidateTx(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	if !ready || candidate.ID != runID {
		return false, nil
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready',workflow_attention=null where id=$1 and revision=$2 and workflow_phase='review-feedback'`, jobID, revision)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

var _ spine.ReviewStore = Store{}
