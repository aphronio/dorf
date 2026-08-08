package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/spine"
)

const dogfoodStartingRevision = "a09e08da11cc89aacd8aee6d33a4a38c45d53824"

const (
	PublicationTaskName        = "dorf-github-publication-v1"
	PublicationTaskMaxAttempts = 5
)

func PublicationTaskKey(jobID, revision string, attempt int) string {
	return fmt.Sprintf("publication:%s:%s:%d", jobID, revision, attempt)
}

// BindDogfoodPublicationAuthority is deliberately limited to the one #43 Job
// admitted before migration 009 existed. It is not a compatibility backfill.
func (s Store) BindDogfoodPublicationAuthority(ctx context.Context, jobID, repository, installation, base string) (spine.Job, error) {
	job, err := s.Job(ctx, jobID)
	if err != nil {
		return spine.Job{}, err
	}
	if job.StartingRevision != dogfoodStartingRevision || base != "greenfield" {
		return spine.Job{}, fmt.Errorf("publication bind is only the guarded #43 dogfood bootstrap at starting Revision %s with base greenfield", dogfoodStartingRevision)
	}
	if err := githubapi.ValidateAuthority(job.Repository, repository, installation, base, job.Branch); err != nil {
		return spine.Job{}, err
	}
	result, err := s.DB.ExecContext(ctx, `
		update dorf.jobs set github_repository=$2,github_installation_id=$3,base_branch=$4
		where id=$1 and github_repository is null and github_installation_id is null and base_branch is null`, jobID, repository, installation, base)
	if err != nil {
		return spine.Job{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return spine.Job{}, err
	}
	if rows == 0 {
		bound, loadErr := s.Job(ctx, jobID)
		if loadErr != nil {
			return spine.Job{}, loadErr
		}
		if bound.GitHubRepository != repository || bound.GitHubInstallation != installation || bound.BaseBranch != base {
			return spine.Job{}, fmt.Errorf("Job publication authority is already bound differently")
		}
		return bound, nil
	}
	return s.Job(ctx, jobID)
}

func (s Store) BeginPublication(ctx context.Context, jobID, revision string) (spine.Job, spine.Action, spine.Action, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, false, err
	}
	defer tx.Rollback()
	var current, phase, repository, installation, base, head, clone, taskID string
	var attempt int
	var admissionOpen bool
	var cleanupState spine.CleanupState
	alreadyPublished := false
	spawn := false
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase,coalesce(github_repository,''),coalesce(github_installation_id,''),coalesce(base_branch,''),branch,repository,publication_attempt,coalesce(publication_task_id,''),admission_open,cleanup_state from dorf.jobs where id=$1 for update`, jobID).Scan(&current, &phase, &repository, &installation, &base, &head, &clone, &attempt, &taskID, &admissionOpen, &cleanupState); err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, false, err
	}
	if !admissionOpen || cleanupState != spine.CleanupPending {
		return spine.Job{}, spine.Action{}, spine.Action{}, false, fmt.Errorf("publication cannot start after Job admission closes or cleanup begins")
	}
	if current != revision || !ValidRevision(revision) {
		return spine.Job{}, spine.Action{}, spine.Action{}, false, fmt.Errorf("publication Revision %s conflicts with exact ready Revision %s", revision, current)
	}
	if err := githubapi.ValidateAuthority(clone, repository, installation, base, head); err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, false, fmt.Errorf("publication authority unresolved: %w", err)
	}
	switch phase {
	case "ready":
		spawn = true
	case "publishing":
		if taskID == "" {
			// BeginPublication commits before Absurd Spawn/Attach. An empty
			// attachment is the recoverable same-attempt scheduling window;
			// the deterministic Absurd key decides create versus adopt.
			spawn = true
			break
		}
		task, err := scanPublicationTask(tx.QueryRowContext(ctx, publicationTaskQuery, taskID))
		if err != nil {
			return spine.Job{}, spine.Action{}, spine.Action{}, false, fmt.Errorf("inspect attached publication task %s: %w", taskID, err)
		}
		exhausted, err := validatePublicationTask(jobID, revision, attempt, task)
		if err != nil {
			return spine.Job{}, spine.Action{}, spine.Action{}, false, err
		}
		if exhausted {
			attempt++
			spawn = true
		}
	case "published":
		var proposed string
		err := tx.QueryRowContext(ctx, `select proposed_revision from dorf.github_proposals where job_id=$1`, jobID).Scan(&proposed)
		if err == nil && proposed == revision {
			alreadyPublished = true
			// The deterministic task may have won the Job fence and completed
			// after Spawn but before its ID was attached. Re-adopt that same
			// attempt key without reopening publication.
			spawn = taskID == ""
		} else {
			return spine.Job{}, spine.Action{}, spine.Action{}, false, fmt.Errorf("published Job is not stale at a later exact ready Revision")
		}
	case "publication-blocked":
		attempt++
		spawn = true
	default:
		return spine.Job{}, spine.Action{}, spine.Action{}, false, fmt.Errorf("exact Revision readiness is required before publication (phase %s)", phase)
	}
	if spawn && !alreadyPublished {
		if _, err := tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='publishing',workflow_attention=null,publication_attempt=$2,publication_task_id=null where id=$1`, jobID, attempt); err != nil {
			return spine.Job{}, spine.Action{}, spine.Action{}, false, err
		}
	}
	push, err := beginPublicationAction(ctx, tx, jobID, spine.ActionRepositoryPush, revision)
	if err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, false, err
	}
	pull, err := beginPublicationAction(ctx, tx, jobID, spine.ActionGitHubPullRequest, revision)
	if err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, false, err
	}
	job, err := s.Job(ctx, jobID)
	return job, push, pull, spawn, err
}

const publicationTaskQuery = `select t.task_id::text,t.task_name,t.state,
	coalesce(t.params->>'job_id',''),coalesce(t.params->>'revision',''),coalesce(t.params->>'attempt',''),
	coalesce(t.idempotency_key,''),coalesce(t.max_attempts,0),t.attempts,
	coalesce(r.state,''),coalesce(r.attempt,0)
	from absurd.t_dorf_jobs t
	left join absurd.r_dorf_jobs r on r.run_id=t.last_attempt_run and r.task_id=t.task_id
	where t.task_id=$1::uuid`

type publicationTaskRecord struct {
	ID, Name, State, JobID, Revision, Attempt, IdempotencyKey string
	MaxAttempts, Attempts, LastRunAttempt                     int
	LastRunState                                              string
}

func scanPublicationTask(row rowScanner) (publicationTaskRecord, error) {
	var task publicationTaskRecord
	err := row.Scan(&task.ID, &task.Name, &task.State, &task.JobID, &task.Revision, &task.Attempt, &task.IdempotencyKey, &task.MaxAttempts, &task.Attempts, &task.LastRunState, &task.LastRunAttempt)
	return task, err
}

func validatePublicationTask(jobID, revision string, attempt int, task publicationTaskRecord) (bool, error) {
	wantKey := PublicationTaskKey(jobID, revision, attempt)
	if task.Name != PublicationTaskName || task.JobID != jobID || task.Revision != revision || task.Attempt != strconv.Itoa(attempt) || task.IdempotencyKey != wantKey || task.MaxAttempts != PublicationTaskMaxAttempts {
		return false, fmt.Errorf("attached Absurd task %s does not prove exact publication authority for Job %s Revision %s attempt %d", task.ID, jobID, revision, attempt)
	}
	switch task.State {
	case "pending", "running", "sleeping":
		if task.Attempts < 1 || task.Attempts > PublicationTaskMaxAttempts || task.LastRunState != task.State || task.LastRunAttempt != task.Attempts {
			return false, fmt.Errorf("attached publication task %s has ambiguous active authority: task=%s/%d last-run=%s/%d", task.ID, task.State, task.Attempts, task.LastRunState, task.LastRunAttempt)
		}
		return false, nil
	case "failed":
		if task.Attempts != PublicationTaskMaxAttempts || task.LastRunState != "failed" || task.LastRunAttempt != PublicationTaskMaxAttempts {
			return false, fmt.Errorf("attached publication task %s has ambiguous failed authority: attempts=%d/%d last-run=%s/%d", task.ID, task.Attempts, task.MaxAttempts, task.LastRunState, task.LastRunAttempt)
		}
		return true, nil
	default:
		return false, fmt.Errorf("attached publication task %s is terminal %s while the Job remains publishing; refusing to create another task", task.ID, task.State)
	}
}

func beginPublicationAction(ctx context.Context, tx *sql.Tx, jobID string, kind spine.ActionKind, revision string) (spine.Action, error) {
	id := spine.ScopedActionID(jobID, kind, revision)
	if _, err := tx.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state,scope_key) values($1,$2,$3,'pending',$4) on conflict do nothing`, id, jobID, kind, revision); err != nil {
		return spine.Action{}, err
	}
	var action spine.Action
	err := tx.QueryRowContext(ctx, `update dorf.actions set attempts=attempts+case when state in ('succeeded','failed') then 0 else 1 end,updated_at=clock_timestamp() where id=$1 and job_id=$2 and kind=$3 and scope_key=$4 returning id,job_id,coalesce(message_id,''),kind,state,coalesce(external_id,''),coalesce(external_outcome,''),scope_key`, id, jobID, kind, revision).Scan(&action.ID, &action.JobID, &action.MessageID, &action.Kind, &action.State, &action.ExternalID, &action.Outcome, &action.Scope)
	return action, err
}

func (s Store) PublicationActions(ctx context.Context, jobID, revision string) (spine.Action, spine.Action, error) {
	load := func(kind spine.ActionKind) (spine.Action, error) {
		var action spine.Action
		err := s.DB.QueryRowContext(ctx, `select id,job_id,coalesce(message_id,''),kind,state,coalesce(external_id,''),coalesce(external_outcome,''),scope_key from dorf.actions where job_id=$1 and kind=$2 and scope_key=$3`, jobID, kind, revision).Scan(&action.ID, &action.JobID, &action.MessageID, &action.Kind, &action.State, &action.ExternalID, &action.Outcome, &action.Scope)
		return action, err
	}
	push, err := load(spine.ActionRepositoryPush)
	if err != nil {
		return spine.Action{}, spine.Action{}, err
	}
	pull, err := load(spine.ActionGitHubPullRequest)
	return push, pull, err
}

func (s Store) AttachPublicationTask(ctx context.Context, jobID, revision string, attempt int, taskID string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set publication_task_id=coalesce(publication_task_id,$4) where id=$1 and revision=$2 and publication_attempt=$3 and workflow_phase in ('publishing','published') and (publication_task_id is null or publication_task_id=$4)`, jobID, revision, attempt, taskID))
}

func (s Store) RecordPush(ctx context.Context, actionID, revision string) error {
	return expectOne(s.DB.ExecContext(ctx, `update dorf.actions set state='succeeded',external_id=$2,external_outcome='remote-head-exact',updated_at=clock_timestamp() where id=$1 and kind='repository-push' and scope_key=$2`, actionID, revision))
}

func (s Store) RecordProposal(ctx context.Context, actionID string, proposal spine.GitHubProposal) error {
	if proposal.Number < 1 || proposal.URL == "" || proposal.BodyDigest == "" || proposal.ProposedRevision != proposal.ObservedRemoteHead {
		return fmt.Errorf("proposal receipt is incomplete or not exact-Revision fresh")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var repository, installation, base, head, revision, phase string
	if err := tx.QueryRowContext(ctx, `select github_repository,github_installation_id,base_branch,branch,revision,workflow_phase from dorf.jobs where id=$1 for update`, proposal.JobID).Scan(&repository, &installation, &base, &head, &revision, &phase); err != nil {
		return err
	}
	if phase != "publishing" || repository != proposal.Repository || installation != proposal.InstallationID || base != proposal.BaseBranch || head != proposal.HeadBranch || revision != proposal.ProposedRevision {
		return fmt.Errorf("proposal receipt conflicts with immutable Job authority or exact current Revision")
	}
	var pushState string
	if err := tx.QueryRowContext(ctx, `select state from dorf.actions where job_id=$1 and kind='repository-push' and scope_key=$2`, proposal.JobID, revision).Scan(&pushState); err != nil || pushState != "succeeded" {
		return fmt.Errorf("proposal cannot be recorded before exact repository push success")
	}
	var existing spine.GitHubProposal
	err = tx.QueryRowContext(ctx, `select repository,installation_id,base_branch,head_branch,pr_number from dorf.github_proposals where job_id=$1`, proposal.JobID).Scan(&existing.Repository, &existing.InstallationID, &existing.BaseBranch, &existing.HeadBranch, &existing.Number)
	if err == nil && (existing.Number != proposal.Number || existing.Repository != proposal.Repository || existing.InstallationID != proposal.InstallationID || existing.BaseBranch != proposal.BaseBranch || existing.HeadBranch != proposal.HeadBranch) {
		return fmt.Errorf("Job already owns conflicting GitHub proposal identity at pull request #%d", existing.Number)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		insert into dorf.github_proposals(job_id,repository,installation_id,base_branch,head_branch,pr_number,pr_url,proposed_revision,observed_remote_head,body_digest)
		values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		on conflict(job_id) do update set
		  pr_url=excluded.pr_url,proposed_revision=excluded.proposed_revision,
		  observed_remote_head=excluded.observed_remote_head,body_digest=excluded.body_digest,
		  observed_at=clock_timestamp()
		where dorf.github_proposals.repository=excluded.repository
		  and dorf.github_proposals.installation_id=excluded.installation_id
		  and dorf.github_proposals.base_branch=excluded.base_branch
		  and dorf.github_proposals.head_branch=excluded.head_branch
		  and dorf.github_proposals.pr_number=excluded.pr_number`, proposal.JobID, proposal.Repository, proposal.InstallationID, proposal.BaseBranch, proposal.HeadBranch, proposal.Number, proposal.URL, proposal.ProposedRevision, proposal.ObservedRemoteHead, proposal.BodyDigest)
	if err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.actions set state='succeeded',external_id=$2,external_outcome=$3,updated_at=clock_timestamp() where id=$1 and job_id=$4 and kind='github-pull-request' and scope_key=$5`, actionID, strconvFormat(proposal.Number), proposal.BodyDigest, proposal.JobID, proposal.ProposedRevision)); err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set workflow_phase='published',workflow_attention=null where id=$1 and revision=$2 and workflow_phase='publishing'`, proposal.JobID, proposal.ProposedRevision)); err != nil {
		return err
	}
	return tx.Commit()
}

func strconvFormat(number int64) string { return fmt.Sprintf("%d", number) }

func (s Store) BlockPublication(ctx context.Context, jobID, revision, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "GitHub publication needs operator attention"
	}
	return expectOne(s.DB.ExecContext(ctx, `update dorf.jobs set workflow_phase='publication-blocked',workflow_attention=$3 where id=$1 and revision=$2 and workflow_phase='publishing'`, jobID, revision, reason))
}

func (s Store) Proposal(ctx context.Context, jobID string) (*spine.GitHubProposal, error) {
	var proposal spine.GitHubProposal
	err := s.DB.QueryRowContext(ctx, `select job_id,repository,installation_id,base_branch,head_branch,pr_number,pr_url,proposed_revision,observed_remote_head,body_digest from dorf.github_proposals where job_id=$1`, jobID).Scan(&proposal.JobID, &proposal.Repository, &proposal.InstallationID, &proposal.BaseBranch, &proposal.HeadBranch, &proposal.Number, &proposal.URL, &proposal.ProposedRevision, &proposal.ObservedRemoteHead, &proposal.BodyDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var current string
	if err := s.DB.QueryRowContext(ctx, `select revision from dorf.jobs where id=$1`, jobID).Scan(&current); err != nil {
		return nil, err
	}
	proposal.Stale = proposal.ProposedRevision != current || proposal.ObservedRemoteHead != current
	return &proposal, nil
}
