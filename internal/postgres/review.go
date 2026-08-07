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
	// Role requests must be bound before the mandatory policy result is
	// computed. Leave an explicit activation boundary after verified Checks so
	// review activate can atomically persist either the implementation's
	// allowlisted requests or the explicit empty set.
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='review-activation',workflow_attention=null where id=$1 and revision=$2 and workflow_phase='checking'`, jobID, revision)); err != nil {
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

func implementationRunID(ctx context.Context, tx *sql.Tx, jobID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `select ar.id from dorf.agent_runs ar join dorf.job_messages m on m.id=ar.message_id where ar.job_id=$1 and ar.role='implement' and ar.state='completed' order by m.sequence limit 1`, jobID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("review requires the completed original implementation AgentRun: %w", err)
	}
	return id, nil
}

func normalizeRequestedRoles(roles []policy.Role) ([]policy.Role, error) {
	seen := map[policy.Role]bool{}
	result := make([]policy.Role, 0, len(roles))
	for _, role := range roles {
		if !policy.Allowed(role) || seen[role] {
			return nil, fmt.Errorf("invalid, unsafe, or duplicate requested review Role %q", role)
		}
		seen[role] = true
		result = append(result, role)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func (s Store) ActivateReview(ctx context.Context, activation spine.ReviewActivation) (spine.ReviewPlanRecord, bool, error) {
	roles, err := normalizeRequestedRoles(activation.RequestedRoles)
	if err != nil {
		_ = s.BlockWorkflow(ctx, activation.JobID, "review activation rejected: "+err.Error())
		return spine.ReviewPlanRecord{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.ReviewPlanRecord{}, false, err
	}
	defer tx.Rollback()
	var revision, phase, cleanupState string
	var reviewRepairCount int
	var admissionOpen bool
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase,admission_open,cleanup_state,review_repair_count from dorf.jobs where id=$1 for update`, activation.JobID).Scan(&revision, &phase, &admissionOpen, &cleanupState, &reviewRepairCount); err != nil {
		return spine.ReviewPlanRecord{}, false, err
	}
	if !admissionOpen || cleanupState != string(spine.CleanupPending) {
		return spine.ReviewPlanRecord{}, false, fmt.Errorf("review activation requires open admission before cleanup")
	}
	if revision != activation.Revision {
		return spine.ReviewPlanRecord{}, false, fmt.Errorf("review activation Revision %s conflicts with current Revision %s", activation.Revision, revision)
	}
	if reviewRepairCount == 1 && len(roles) > 0 {
		return spine.ReviewPlanRecord{}, false, fmt.Errorf("repaired Revision review activation cannot replay optional requested Roles")
	}
	requestedBy := strings.TrimSpace(activation.RequestedByRunID)
	if len(roles) > 0 {
		original, err := implementationRunID(ctx, tx, activation.JobID)
		if err != nil {
			return spine.ReviewPlanRecord{}, false, err
		}
		if requestedBy == "" {
			requestedBy = original
		}
		if requestedBy != original {
			return spine.ReviewPlanRecord{}, false, fmt.Errorf("review request must be attributed to original implementation AgentRun %s", original)
		}
	} else {
		requestedBy = ""
	}
	encoded, _ := json.Marshal(roles)
	result, err := tx.ExecContext(ctx, `insert into dorf.review_plans(job_id,revision,state,requested_roles,requested_by_run_id) values($1,$2,'pending',$3::jsonb,nullif($4,'')) on conflict do nothing`, activation.JobID, revision, encoded, requestedBy)
	if err != nil {
		return spine.ReviewPlanRecord{}, false, err
	}
	createdRows, _ := result.RowsAffected()
	stored, err := reviewPlanTx(ctx, tx, activation.JobID, revision)
	if err != nil {
		return spine.ReviewPlanRecord{}, false, err
	}
	if !slices.Equal(stored.RequestedRoles, roles) || stored.RequestedByRunID != requestedBy {
		return spine.ReviewPlanRecord{}, false, fmt.Errorf("review activation is already bound to different immutable requested Roles or attribution")
	}
	if createdRows == 1 {
		if phase != "review-activation" {
			return spine.ReviewPlanRecord{}, false, fmt.Errorf("new review activation is not admissible during workflow phase %s", phase)
		}
		if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='review-planning',workflow_attention=null where id=$1 and revision=$2 and workflow_phase='review-activation'`, activation.JobID, revision)); err != nil {
			return spine.ReviewPlanRecord{}, false, err
		}
	} else if phase == "review-activation" {
		return spine.ReviewPlanRecord{}, false, fmt.Errorf("persisted review activation did not advance atomically")
	}
	if err := tx.Commit(); err != nil {
		return spine.ReviewPlanRecord{}, false, err
	}
	return stored, createdRows == 1, nil
}

func (s Store) ReviewPlan(ctx context.Context, jobID, revision string) (spine.ReviewPlanRecord, error) {
	return reviewPlanRow(s.DB.QueryRowContext(ctx, `select job_id,revision,state,coalesce(facts,'{}'::jsonb),coalesce(initial_policy,'{}'::jsonb),coalesce(final_plan,'{}'::jsonb),coalesce(policy_digest,''),requested_roles,coalesce(requested_by_run_id,''),coalesce(triage_run_id,''),coalesce(triage_rationale,''),created_at,finalized_at from dorf.review_plans where job_id=$1 and revision=$2`, jobID, revision))
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
	return reviewPlanRow(tx.QueryRowContext(ctx, `select job_id,revision,state,coalesce(facts,'{}'::jsonb),coalesce(initial_policy,'{}'::jsonb),coalesce(final_plan,'{}'::jsonb),coalesce(policy_digest,''),requested_roles,coalesce(requested_by_run_id,''),coalesce(triage_run_id,''),coalesce(triage_rationale,''),created_at,finalized_at from dorf.review_plans where job_id=$1 and revision=$2 for update`, jobID, revision))
}

func reviewPlanRow(row *sql.Row) (spine.ReviewPlanRecord, error) {
	var record spine.ReviewPlanRecord
	var facts, initial, final, requested []byte
	var finalized sql.NullTime
	err := row.Scan(&record.JobID, &record.Revision, &record.State, &facts, &initial, &final, &record.PolicyDigest, &requested, &record.RequestedByRunID, &record.TriageRunID, &record.TriageRationale, &record.CreatedAt, &finalized)
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
	if string(initial) != "{}" {
		if err := json.Unmarshal(initial, &record.Initial); err != nil {
			return record, err
		}
	}
	if string(final) != "{}" {
		if err := json.Unmarshal(final, &record.Final); err != nil {
			return record, err
		}
	}
	if err := json.Unmarshal(requested, &record.RequestedRoles); err != nil {
		return record, err
	}
	return record, nil
}

func policyDigest(facts policy.ChangeFacts, plan policy.ReviewPlan, requested []policy.Role, requestedBy string) (string, error) {
	contents, err := json.Marshal(struct {
		Facts       policy.ChangeFacts `json:"facts"`
		Plan        policy.ReviewPlan  `json:"plan"`
		Requested   []policy.Role      `json:"requested_roles"`
		RequestedBy string             `json:"requested_by"`
	}{facts, plan, requested, requestedBy})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:]), nil
}

func (s Store) RecordReviewPolicy(ctx context.Context, proposed spine.ReviewPlanRecord) error {
	digest, err := policyDigest(proposed.Facts, proposed.Initial, proposed.RequestedRoles, proposed.RequestedByRunID)
	if err != nil {
		return err
	}
	if proposed.Facts.Revision != proposed.Revision || proposed.Initial.NeedsTriage != (proposed.Initial.Decision == "triage") {
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
	if stored.State != "pending" || !slices.Equal(stored.RequestedRoles, proposed.RequestedRoles) || stored.RequestedByRunID != proposed.RequestedByRunID {
		return fmt.Errorf("review policy conflicts with its durable activation")
	}
	factsJSON, _ := json.Marshal(proposed.Facts)
	initialJSON, _ := json.Marshal(proposed.Initial)
	final := proposed.Initial
	state, phase := "final", "reviewing"
	if proposed.Initial.Decision == "triage" {
		state, phase = "triage-pending", "review-triage"
		final = policy.ReviewPlan{}
	} else if proposed.Initial.Decision == "no-review" {
		phase = "ready"
	}
	finalJSON, _ := json.Marshal(final)
	if _, err := tx.ExecContext(ctx, `update dorf.review_plans set state=$3,facts=$4::jsonb,initial_policy=$5::jsonb,final_plan=$6::jsonb,policy_digest=$7,finalized_at=case when $3='final' then clock_timestamp() else null end where job_id=$1 and revision=$2 and policy_digest is null`, proposed.JobID, proposed.Revision, state, factsJSON, initialJSON, finalJSON, digest); err != nil {
		return err
	}
	if proposed.Initial.Decision == "triage" {
		runID, err := createReviewRunTx(ctx, tx, proposed.JobID, proposed.Revision, spine.ReviewTriageRole, proposed.Facts)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `update dorf.review_plans set triage_run_id=$3 where job_id=$1 and revision=$2`, proposed.JobID, proposed.Revision, runID); err != nil {
			return err
		}
	} else {
		for _, role := range proposed.Initial.Roles {
			if _, err := createReviewRunTx(ctx, tx, proposed.JobID, proposed.Revision, string(role), proposed.Facts); err != nil {
				return err
			}
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
	input, output := policy.RolePrompt(policy.Role(role), facts, declaredChecks), policy.FindingOutputContract
	if role == spine.ReviewTriageRole {
		input, output = policy.TriagePrompt(facts), policy.TriageOutputContract
	}
	turnAction := spine.ScopedActionID(jobID, spine.ActionTurnStart, runID)
	sessionAction := spine.ScopedActionID(jobID, spine.ActionSessionStart, runID)
	sandboxCreateAction := spine.ScopedActionID(jobID, spine.ActionSandboxCreate, runID)
	routeCreateAction := spine.ScopedActionID(jobID, spine.ActionRouteCreate, runID)
	createAction := spine.ScopedActionID(jobID, spine.ActionReviewWorkspaceCreate, runID)
	deleteAction := spine.ScopedActionID(jobID, spine.ActionReviewWorkspaceDelete, runID)
	routeRevokeAction := spine.ScopedActionID(jobID, spine.ActionRouteRevoke, runID)
	sandboxDeleteAction := spine.ScopedActionID(jobID, spine.ActionSandboxDelete, runID)
	for _, action := range []struct {
		id   string
		kind spine.ActionKind
	}{{turnAction, spine.ActionTurnStart}, {sessionAction, spine.ActionSessionStart}, {sandboxCreateAction, spine.ActionSandboxCreate}, {routeCreateAction, spine.ActionRouteCreate}, {createAction, spine.ActionReviewWorkspaceCreate}, {deleteAction, spine.ActionReviewWorkspaceDelete}, {routeRevokeAction, spine.ActionRouteRevoke}, {sandboxDeleteAction, spine.ActionSandboxDelete}} {
		if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state,scope_key) values($1,$2,$3,'pending',$4) on conflict do nothing`, action.id, jobID, action.kind, runID); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,action_id,role,state,revision,capability,workspace,input_contract,output_contract) values($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9) on conflict do nothing`, runID, jobID, turnAction, role, revision, spine.ReviewReadOnlyCapability, workspace, input, output); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.review_workspaces(run_id,job_id,revision,path,create_action_id,delete_action_id) values($1,$2,$3,$4,$5,$6) on conflict do nothing`, runID, jobID, revision, workspace, createAction, deleteAction); err != nil {
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

func (s Store) ReviewRun(ctx context.Context, runID string) (spine.AgentRun, error) {
	return scanReviewRun(s.DB.QueryRowContext(ctx, reviewRunSelect+` where ar.id=$1`, runID))
}

const reviewRunSelect = `select ar.id,ar.job_id,coalesce(ar.message_id,''),ar.action_id,coalesce(ar.session_id,''),ar.state,ar.baseline_native_turn_id is not null,coalesce(ar.baseline_native_turn_id,''),coalesce(ar.native_turn_id,''),coalesce(ar.native_outcome,''),coalesce(ar.attention,''),ar.role,coalesce(ar.revision,''),coalesce(ar.capability,''),coalesce(ar.workspace,''),coalesce(ar.input_contract,''),coalesce(ar.output_contract,''),coalesce(ar.claim_evidence_id,''),coalesce(ar.observed_evidence_id,''),ar.started_at,ar.finished_at,ar.input_tokens,ar.cached_input_tokens,ar.output_tokens,ar.cost_microusd,ar.usage_available,ar.yield_count,coalesce(rr.sandbox_name,''),coalesce(rr.route_id,''),coalesce(rr.app_server_id,''),coalesce(rr.ownership_nonce,''),coalesce(rr.submission_nonce,''),coalesce(rr.input_digest,''),coalesce(rr.revision_tree,''),coalesce(rr.sandbox_state,''),coalesce(rr.route_state,''),coalesce(rr.checkout_state,''),coalesce(rr.post_review_state,'') from dorf.agent_runs ar left join dorf.review_resources rr on rr.run_id=ar.id`

func scanReviewRun(row rowScanner) (spine.AgentRun, error) {
	var run spine.AgentRun
	var started, finished sql.NullTime
	err := row.Scan(&run.ID, &run.JobID, &run.MessageID, &run.ActionID, &run.SessionID, &run.State, &run.BaselineRecorded, &run.BaselineTurnID, &run.NativeTurnID, &run.NativeOutcome, &run.Attention, &run.Role, &run.Revision, &run.Capability, &run.Workspace, &run.InputContract, &run.OutputContract, &run.ClaimEvidenceID, &run.ObservedEvidenceID, &started, &finished, &run.InputTokens, &run.CachedInputTokens, &run.OutputTokens, &run.CostMicrousd, &run.UsageAvailable, &run.YieldCount, &run.ReviewerSandboxID, &run.ReviewerRouteID, &run.ReviewerAppServer, &run.ReviewerOwnerNonce, &run.SubmissionNonce, &run.InputDigest, &run.RevisionTree, &run.ReviewerSandboxState, &run.ReviewerRouteState, &run.CheckoutState, &run.PostReviewState)
	if started.Valid {
		run.StartedAt = started.Time
	}
	if finished.Valid {
		run.FinishedAt = finished.Time
	}
	return run, err
}

func (s Store) ReviewRuns(ctx context.Context, jobID, revision string) ([]spine.ReviewRunView, error) {
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
		view := spine.ReviewRunView{AgentRun: run}
		var material, stale bool
		var summary, rationale, evidenceID, adjudication string
		var affectedRoles, affectedChecks []byte
		err = s.DB.QueryRowContext(ctx, `select material,summary,rationale,affected_roles,affected_checks,evidence_id,adjudication,stale from dorf.review_findings where run_id=$1`, run.ID).Scan(&material, &summary, &rationale, &affectedRoles, &affectedChecks, &evidenceID, &adjudication, &stale)
		if err == nil {
			finding := &spine.ReviewFinding{RunID: run.ID, Revision: run.Revision, Role: policy.Role(run.Role), Material: material, Summary: summary, Rationale: rationale, EvidenceID: evidenceID, Adjudication: adjudication, Stale: stale}
			_ = json.Unmarshal(affectedRoles, &finding.AffectedRoles)
			_ = json.Unmarshal(affectedChecks, &finding.AffectedChecks)
			view.Finding, view.Stale = finding, stale
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err := s.DB.QueryRowContext(ctx, `select state,delete_action_id from dorf.review_workspaces where run_id=$1`, run.ID).Scan(&view.WorkspaceState, &view.CleanupActionID); err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (s Store) AllReviewRuns(ctx context.Context, jobID string) ([]spine.ReviewRunView, error) {
	var currentRevision string
	if err := s.DB.QueryRowContext(ctx, `select revision from dorf.jobs where id=$1`, jobID).Scan(&currentRevision); err != nil {
		return nil, err
	}
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
		for i := range runs {
			if runs[i].Revision != currentRevision {
				runs[i].Stale = true
			}
		}
		result = append(result, runs...)
	}
	return result, nil
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
		err = tx.QueryRowContext(ctx, `select create_action_id from dorf.review_workspaces where run_id=$1`, runID).Scan(&actionID)
	case spine.ActionReviewWorkspaceDelete:
		err = tx.QueryRowContext(ctx, `select delete_action_id from dorf.review_workspaces where run_id=$1`, runID).Scan(&actionID)
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
	err = tx.QueryRowContext(ctx, `update dorf.actions set attempts=attempts+case when state in ('succeeded','failed') then 0 else 1 end,updated_at=clock_timestamp() where id=$1 returning id,job_id,coalesce(message_id,''),kind,state,coalesce(external_id,''),coalesce(external_outcome,''),scope_key`, actionID).Scan(&action.ID, &action.JobID, &action.MessageID, &action.Kind, &action.State, &action.ExternalID, &action.Outcome, &action.Scope)
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
	if err := expectOne(tx.ExecContext(ctx, `update dorf.actions set state='uncertain',external_outcome=$3,updated_at=clock_timestamp() where id=$1 and scope_key=$2 and kind='codex-session-start' and state in ('pending','uncertain')`, sessionActionID, runID, outcome)); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.actions set state='uncertain',external_outcome=$2,updated_at=clock_timestamp() where id=$1 and state in ('pending','uncertain')`, turnActionID, outcome)); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.agent_runs set state='uncertain',attention=$2,updated_at=clock_timestamp() where id=$1 and session_id is null and native_turn_id is null`, runID, reason)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s Store) BeginReviewWorkspaceCleanup(ctx context.Context, runID string) (spine.Action, error) {
	return s.beginReviewAction(ctx, runID, spine.ActionReviewWorkspaceDelete)
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
	if _, err := tx.ExecContext(ctx, `update dorf.actions set state='failed',external_outcome=$2,updated_at=clock_timestamp() where id=$1 and state in ('pending','uncertain')`, actionID, reason); err != nil {
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

func (s Store) RecordTriageResult(ctx context.Context, runID string, outcome spine.NativeTurn, claim, observed spine.Evidence, final policy.ReviewPlan, rationale string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var jobID, revision, role, runState string
	if err := tx.QueryRowContext(ctx, `select job_id,revision,role,state from dorf.agent_runs where id=$1 for update`, runID).Scan(&jobID, &revision, &role, &runState); err != nil {
		return err
	}
	if role != spine.ReviewTriageRole || runState != "completed" || outcome.Status != "completed" || claim.Revision != revision || observed.Revision != revision {
		return fmt.Errorf("triage result conflicts with its AgentRun or Revision")
	}
	var boundaryReady bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from dorf.review_resources where run_id=$1 and sandbox_state='created' and route_state='active' and checkout_state='verified' and post_review_state='verified' and app_server_id is not null and revision_tree is not null)`, runID).Scan(&boundaryReady); err != nil || !boundaryReady {
		return fmt.Errorf("triage result lacks an attested isolated reviewer boundary")
	}
	plan, err := reviewPlanTx(ctx, tx, jobID, revision)
	if err != nil {
		return err
	}
	if plan.State != "triage-pending" || plan.TriageRunID != runID {
		return fmt.Errorf("triage is not pending for AgentRun %s", runID)
	}
	if err := insertEvidence(ctx, tx, jobID, claim); err != nil {
		return err
	}
	if err := insertEvidence(ctx, tx, jobID, observed); err != nil {
		return err
	}
	finalJSON, _ := json.Marshal(final)
	if _, err := tx.ExecContext(ctx, `update dorf.agent_runs set claim_evidence_id=$2,observed_evidence_id=$3,input_tokens=$4,cached_input_tokens=$5,output_tokens=$6,cost_microusd=$7,usage_available=$8,finished_at=coalesce(finished_at,clock_timestamp()) where id=$1`, runID, claim.ID, observed.ID, outcome.InputTokens, outcome.CachedInputTokens, outcome.OutputTokens, outcome.CostMicrousd, outcome.UsageAvailable); err != nil {
		return err
	}
	for _, selected := range final.Roles {
		if _, err := createReviewRunTx(ctx, tx, jobID, revision, string(selected), plan.Facts); err != nil {
			return err
		}
	}
	phase := "reviewing"
	if final.Decision == "no-review" {
		phase = "ready"
	}
	if _, err := tx.ExecContext(ctx, `update dorf.review_plans set state='final',final_plan=$3::jsonb,triage_rationale=$4,finalized_at=clock_timestamp() where job_id=$1 and revision=$2`, jobID, revision, finalJSON, rationale); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase=$3 where id=$1 and revision=$2 and workflow_phase='review-triage'`, jobID, revision, phase)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) RecordReviewResult(ctx context.Context, runID string, outcome spine.NativeTurn, claim, observed spine.Evidence, finding spine.ReviewFinding) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var jobID, revision, role, state string
	if err := tx.QueryRowContext(ctx, `select job_id,revision,role,state from dorf.agent_runs where id=$1 for update`, runID).Scan(&jobID, &revision, &role, &state); err != nil {
		return err
	}
	if role == spine.ReviewTriageRole || outcome.Status != "completed" || state != "completed" || finding.RunID != runID || finding.Revision != revision || string(finding.Role) != role || claim.Revision != revision || observed.Revision != revision {
		return fmt.Errorf("review result conflicts with its terminal AgentRun or Revision")
	}
	var boundaryReady bool
	if err := tx.QueryRowContext(ctx, `select exists(select 1 from dorf.review_resources where run_id=$1 and sandbox_state='created' and route_state='active' and checkout_state='verified' and post_review_state='verified' and app_server_id is not null and revision_tree is not null)`, runID).Scan(&boundaryReady); err != nil || !boundaryReady {
		return fmt.Errorf("review result lacks an attested isolated reviewer boundary")
	}
	if err := insertEvidence(ctx, tx, jobID, claim); err != nil {
		return err
	}
	if err := insertEvidence(ctx, tx, jobID, observed); err != nil {
		return err
	}
	affectedRoles, _ := json.Marshal(finding.AffectedRoles)
	affectedChecks, _ := json.Marshal(finding.AffectedChecks)
	adjudication := "not-needed"
	if finding.Material {
		adjudication = "pending"
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.review_findings(run_id,job_id,revision,role,material,summary,rationale,affected_roles,affected_checks,evidence_id,adjudication) values($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10,$11) on conflict(run_id) do nothing`, runID, jobID, revision, role, finding.Material, finding.Summary, finding.Rationale, affectedRoles, affectedChecks, claim.ID, adjudication); err != nil {
		return err
	}
	yield := 0
	if finding.Material {
		yield = 1
	}
	if _, err := tx.ExecContext(ctx, `update dorf.agent_runs set claim_evidence_id=$2,observed_evidence_id=$3,input_tokens=$4,cached_input_tokens=$5,output_tokens=$6,cost_microusd=$7,yield_count=$8,usage_available=$9,finished_at=coalesce(finished_at,clock_timestamp()) where id=$1`, runID, claim.ID, observed.ID, outcome.InputTokens, outcome.CachedInputTokens, outcome.OutputTokens, outcome.CostMicrousd, yield, outcome.UsageAvailable); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) AdmitReviewRepair(ctx context.Context, jobID, findingRunID string) (spine.Message, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Message{}, false, err
	}
	defer tx.Rollback()
	var revision, phase, source string
	var count int
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase,review_repair_count,coalesce(review_repair_source_run_id,'') from dorf.jobs where id=$1 for update`, jobID).Scan(&revision, &phase, &count, &source); err != nil {
		return spine.Message{}, false, err
	}
	callerID := "dorf:review-repair:1"
	var existing spine.Message
	err = tx.QueryRowContext(ctx, `select id,job_id,caller_id,sequence,input from dorf.job_messages where job_id=$1 and caller_id=$2`, jobID, callerID).Scan(&existing.ID, &existing.JobID, &existing.CallerID, &existing.Sequence, &existing.Input)
	if err == nil {
		if source != findingRunID {
			return spine.Message{}, false, fmt.Errorf("review repair is already bound to another finding")
		}
		if err := tx.Commit(); err != nil {
			return spine.Message{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spine.Message{}, false, err
	}
	var role, summary, rationale, evidenceID, adjudication string
	var material bool
	if err := tx.QueryRowContext(ctx, `select role,material,summary,rationale,evidence_id,adjudication from dorf.review_findings where run_id=$1 and job_id=$2 and revision=$3 for update`, findingRunID, jobID, revision).Scan(&role, &material, &summary, &rationale, &evidenceID, &adjudication); err != nil {
		return spine.Message{}, false, err
	}
	var materialCount int
	if err := tx.QueryRowContext(ctx, `select count(*) from dorf.review_findings where job_id=$1 and revision=$2 and material`, jobID, revision).Scan(&materialCount); err != nil {
		return spine.Message{}, false, err
	}
	if phase != "reviewing" || count != 0 || !material || adjudication != "pending" || materialCount != 1 {
		return spine.Message{}, false, fmt.Errorf("exactly one unsettled material finding is required for the bounded review repair")
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `select coalesce(max(sequence),0)+1 from dorf.job_messages where job_id=$1`, jobID).Scan(&sequence); err != nil {
		return spine.Message{}, false, err
	}
	input := fmt.Sprintf("Adjudicate the single material %s review claim for exact Revision %s. Claim Evidence %s. Summary: %s. Rationale: %s. If valid, make only the focused repair in the original implementation workspace. If it is a false positive, leave the checkout byte-clean and explain why. Do not commit; return control to Dorf for observed Git and targeted verification.", role, revision, evidenceID, summary, rationale)
	message := spine.Message{ID: spine.MessageID(jobID, callerID), JobID: jobID, CallerID: callerID, Sequence: sequence, Input: input}
	if _, err := tx.ExecContext(ctx, `insert into dorf.job_messages(id,job_id,caller_id,sequence,input) values($1,$2,$3,$4,$5)`, message.ID, jobID, callerID, sequence, input); err != nil {
		return spine.Message{}, false, err
	}
	actionID, runID := spine.TurnActionID(message.ID), spine.AgentRunID(message.ID)
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,message_id,kind,state) values($1,$2,$3,$4,'pending')`, actionID, jobID, message.ID, spine.ActionTurnStart); err != nil {
		return spine.Message{}, false, err
	}
	if err := expectOne(tx.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,message_id,action_id,session_id,role,state) select $1,$2,$3,$4,native_session_id,'repair','pending' from dorf.sessions where job_id=$2`, runID, jobID, message.ID, actionID)); err != nil {
		return spine.Message{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.review_findings set adjudication='pending' where run_id=$1`, findingRunID); err != nil {
		return spine.Message{}, false, err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set review_repair_count=1,review_repair_source_run_id=$2,workflow_phase='review-repairing',workflow_attention=null where id=$1`, jobID, findingRunID)); err != nil {
		return spine.Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Message{}, false, err
	}
	return message, true, nil
}

func (s Store) MarkReviewReady(ctx context.Context, jobID, revision string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	plan, err := reviewPlanTx(ctx, tx, jobID, revision)
	if err != nil {
		return err
	}
	if plan.State != "final" || plan.Final.Decision == "triage" {
		return fmt.Errorf("review plan is not final")
	}
	for _, role := range plan.Final.Roles {
		var state string
		var material bool
		var adjudication string
		err := tx.QueryRowContext(ctx, `select ar.state,rf.material,rf.adjudication from dorf.agent_runs ar join dorf.review_findings rf on rf.run_id=ar.id join dorf.review_resources rr on rr.run_id=ar.id where ar.job_id=$1 and ar.revision=$2 and ar.role=$3 and rr.checkout_state='verified' and rr.post_review_state='verified'`, jobID, revision, role).Scan(&state, &material, &adjudication)
		if err != nil || state != "completed" || material && adjudication != "rejected" {
			return fmt.Errorf("selected review Role %s is not settled", role)
		}
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready',workflow_attention=null where id=$1 and revision=$2 and workflow_phase='reviewing'`, jobID, revision)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) ReviewRepairTargets(ctx context.Context, jobID string) ([]policy.Role, error) {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `select rf.affected_roles from dorf.jobs j join dorf.review_findings rf on rf.run_id=j.review_repair_source_run_id where j.id=$1 and j.review_repair_count=1`, jobID).Scan(&raw)
	if err != nil {
		return nil, err
	}
	var roles []policy.Role
	if err := json.Unmarshal(raw, &roles); err != nil {
		return nil, err
	}
	return roles, nil
}

func (s Store) RejectReviewFinding(ctx context.Context, jobID, revision string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRevision, phase, source string
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase,coalesce(review_repair_source_run_id,'') from dorf.jobs where id=$1 for update`, jobID).Scan(&currentRevision, &phase, &source); err != nil {
		return err
	}
	if currentRevision != revision || phase != "review-repairing" || source == "" {
		return fmt.Errorf("review finding rejection is not admissible for the current Revision and phase")
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.review_findings set adjudication='rejected' where run_id=$1 and revision=$2 and adjudication='pending'`, source, revision)); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='reviewing',workflow_attention=null where id=$1 and revision=$2`, jobID, revision)); err != nil {
		return err
	}
	return tx.Commit()
}

var _ spine.ReviewStore = Store{}
