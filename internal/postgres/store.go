package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var ErrNotFound = errors.New("Dorf Job not found")
var ErrRevisionObservationSuperseded = errors.New("Revision observation is no longer current; retry derived workflow")
var fullCommitOID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
var sha256Digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

const (
	AbsurdReleaseCommit = "550d3b9e6f9382d96178de6ab8c90c7f8edf2227"
	AbsurdSchemaURL     = "https://raw.githubusercontent.com/earendil-works/absurd/" + AbsurdReleaseCommit + "/sql/absurd.sql"
	AbsurdSchemaSHA256  = "d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab"
	initialFromID       = "dorf:initial"
)

var dorfMigrations = []string{"001_baseline.sql"}

type Store struct{ DB *sql.DB }

func (s Store) AbsurdReady(ctx context.Context) (bool, error) {
	var installed bool
	if err := s.DB.QueryRowContext(ctx, `select to_regprocedure('absurd.get_schema_version()') is not null`).Scan(&installed); err != nil {
		return false, err
	}
	if !installed {
		return false, nil
	}
	var version string
	if err := s.DB.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version); err != nil {
		return false, err
	}
	if version != "0.5.0" {
		return false, fmt.Errorf("Absurd schema version is %q; Dorf requires 0.5.0", version)
	}
	return true, nil
}

type admissionInput struct {
	core.JobAdmission
	Repository          string
	Revision            string
	Branch              string
	GitHubRepository    string
	GitHubInstallation  string
	BaseBranch          string
	InvestigationSource investigation.Source
}

func (s Store) BootstrapAbsurd(ctx context.Context, schema []byte) error {
	sum := fmt.Sprintf("%x", sha256.Sum256(schema))
	if sum != AbsurdSchemaSHA256 {
		return fmt.Errorf("Absurd schema checksum is %s; expected pinned 0.5.0 checksum %s", sum, AbsurdSchemaSHA256)
	}
	var installed bool
	if err := s.DB.QueryRowContext(ctx, `select to_regprocedure('absurd.get_schema_version()') is not null`).Scan(&installed); err != nil {
		return err
	}
	if !installed {
		if _, err := s.DB.ExecContext(ctx, string(schema)); err != nil {
			return fmt.Errorf("initialize Absurd 0.5.0 schema: %w", err)
		}
	}
	var version string
	if err := s.DB.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version); err != nil {
		return err
	}
	if version != "0.5.0" {
		return fmt.Errorf("Absurd schema version is %q; expected 0.5.0", version)
	}
	return nil
}

func (s Store) Migrate(ctx context.Context) error {
	var version string
	if err := s.DB.QueryRowContext(ctx, `select absurd.get_schema_version()`).Scan(&version); err != nil {
		return fmt.Errorf("Absurd schema is not ready: %w (initialize pinned Absurd 0.5.0 first)", err)
	}
	if version != "0.5.0" {
		return fmt.Errorf("Absurd schema version is %q; Dorf requires 0.5.0", version)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(hashtextextended('dorf-schema-baseline',0))`); err != nil {
		return err
	}
	var installed bool
	if err := tx.QueryRowContext(ctx, `select to_regnamespace('dorf') is not null`).Scan(&installed); err != nil {
		return err
	}
	applied := map[string]bool{}
	if installed {
		var migrationsTable bool
		if err := tx.QueryRowContext(ctx, `select to_regclass('dorf.schema_migrations') is not null`).Scan(&migrationsTable); err != nil {
			return err
		}
		if !migrationsTable {
			return fmt.Errorf("existing Dorf schema has no baseline identity; recreate this prototype database")
		}
		rows, err := tx.QueryContext(ctx, `select name from dorf.schema_migrations order by name`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			applied[name] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !applied["001_baseline.sql"] {
			return fmt.Errorf("existing Dorf schema has no baseline identity; recreate this prototype database")
		}
		for name := range applied {
			known := false
			for _, migration := range dorfMigrations {
				known = known || name == migration
			}
			if !known {
				return fmt.Errorf("Dorf migration history contains unsupported migration %q", name)
			}
		}
	}
	for _, name := range dorfMigrations {
		if applied[name] {
			continue
		}
		contents, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			return fmt.Errorf("apply Dorf migration %s: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	client, err := absurd.New(absurd.Options{DB: s.DB, QueueName: "dorf_jobs"})
	if err != nil {
		return err
	}
	if err := client.CreateQueue(ctx, "dorf_jobs"); err != nil {
		return fmt.Errorf("create Absurd queue dorf_jobs: %w", err)
	}
	return nil
}

func ValidRevision(value string) bool { return fullCommitOID.MatchString(value) }

func (s Store) AdmitCoding(ctx context.Context, input coding.Admission) (core.Job, bool, error) {
	return s.admit(ctx, admissionInput{
		JobAdmission: input.JobAdmission, Repository: input.Repository, Revision: input.Revision, Branch: input.Branch,
		GitHubRepository: input.GitHubRepository, GitHubInstallation: input.GitHubInstallation, BaseBranch: input.BaseBranch,
	})
}

func (s Store) AdmitInvestigation(ctx context.Context, input investigation.Admission) (core.Job, bool, error) {
	return s.admit(ctx, admissionInput{JobAdmission: input.JobAdmission, InvestigationSource: input.Source})
}

func (s Store) admit(ctx context.Context, input admissionInput) (core.Job, bool, error) {
	input.AdmissionKey = strings.TrimSpace(input.AdmissionKey)
	input.Workflow = core.WorkflowName(strings.TrimSpace(string(input.Workflow)))
	input.WorkflowRevision = strings.TrimSpace(input.WorkflowRevision)
	if input.Workflow == "" {
		input.Workflow = coding.Workflow
	}
	if input.WorkflowRevision == "" && input.Workflow == coding.Workflow {
		input.WorkflowRevision = coding.WorkflowRevision
	}
	input.Repository = strings.TrimSpace(input.Repository)
	input.Revision = strings.TrimSpace(input.Revision)
	input.Branch = strings.TrimSpace(input.Branch)
	input.SandboxProfile = strings.TrimSpace(input.SandboxProfile)
	input.ProviderConnection = strings.TrimSpace(input.ProviderConnection)
	input.Model = strings.TrimSpace(input.Model)
	input.ReasoningEffort = strings.TrimSpace(input.ReasoningEffort)
	input.GitHubRepository = strings.TrimSpace(input.GitHubRepository)
	input.GitHubInstallation = strings.TrimSpace(input.GitHubInstallation)
	input.BaseBranch = strings.TrimSpace(input.BaseBranch)
	input.InvestigationSource = normalizeInvestigationSource(input.InvestigationSource)
	if input.AdmissionKey == "" || input.WorkflowRevision == "" || strings.TrimSpace(input.Goal) == "" || input.SandboxProfile == "" || input.ProviderConnection == "" || input.Model == "" {
		return core.Job{}, false, fmt.Errorf("admission requires key, workflow revision, complete goal, Sandbox profile, AI connection, and model")
	}
	switch input.Workflow {
	case coding.Workflow:
		if input.WorkflowRevision != coding.WorkflowRevision || input.Repository == "" || input.Branch == "" || input.GitHubRepository == "" || input.GitHubInstallation == "" || input.BaseBranch == "" || input.InvestigationSource != (investigation.Source{}) {
			return core.Job{}, false, fmt.Errorf("coding-to-proposal admission requires workflow revision %s, canonical GitHub repository, installation, and explicit base branch", coding.WorkflowRevision)
		}
		if err := githubapi.ValidateAuthority(input.Repository, input.GitHubRepository, input.GitHubInstallation, input.BaseBranch, input.Branch); err != nil {
			return core.Job{}, false, err
		}
	case investigation.Workflow:
		if input.WorkflowRevision != investigation.WorkflowRevision {
			return core.Job{}, false, fmt.Errorf("codebase-investigation admission requires workflow revision %s", investigation.WorkflowRevision)
		}
		if input.GitHubRepository != "" || input.GitHubInstallation != "" || input.BaseBranch != "" {
			return core.Job{}, false, fmt.Errorf("codebase-investigation does not accept GitHub publication authority")
		}
		if err := validateInvestigationSource(input.InvestigationSource); err != nil {
			return core.Job{}, false, err
		}
	default:
		return core.Job{}, false, fmt.Errorf("unsupported workflow %q", input.Workflow)
	}
	revision := input.Revision
	if input.Workflow == investigation.Workflow {
		revision = input.InvestigationSource.Revision
	}
	if !ValidRevision(revision) {
		return core.Job{}, false, fmt.Errorf("admitted revision must be a lowercase full commit OID (40 hex for SHA-1 or 64 hex for SHA-256)")
	}
	if input.ReasoningEffort != "low" && input.ReasoningEffort != "medium" && input.ReasoningEffort != "high" && input.ReasoningEffort != "xhigh" {
		return core.Job{}, false, fmt.Errorf("reasoning effort must be low, medium, high, or xhigh")
	}
	id := core.JobID(input.AdmissionKey)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.Job{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	storedRow, err := queries.GetAdmittedJobForUpdate(ctx, input.AdmissionKey)
	var rows int64
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := queries.LockVerifiedSandboxProfileForAdmission(ctx, dbsql.LockVerifiedSandboxProfileForAdmissionParams{
			Name: input.SandboxProfile, ContractVersion: core.BaseProfileContract,
		}); errors.Is(err, sql.ErrNoRows) {
			return core.Job{}, false, fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", input.SandboxProfile, core.BaseProfileContract)
		} else if err != nil {
			return core.Job{}, false, err
		}
		rows, err = queries.InsertAdmittedJob(ctx, dbsql.InsertAdmittedJobParams{
			ID: id, AdmissionKey: input.AdmissionKey, WorkflowName: input.Workflow, WorkflowRevision: input.WorkflowRevision,
			Goal: input.Goal, SandboxProfile: input.SandboxProfile, ProviderConnection: input.ProviderConnection,
			Model: input.Model, ReasoningEffort: input.ReasoningEffort,
		})
		if err != nil {
			return core.Job{}, false, err
		}
		storedRow, err = queries.GetAdmittedJobForUpdate(ctx, input.AdmissionKey)
	}
	if err != nil {
		return core.Job{}, false, err
	}
	stored := admissionInput{JobAdmission: core.JobAdmission{
		AdmissionKey: storedRow.AdmissionKey, Workflow: core.WorkflowName(storedRow.WorkflowName), WorkflowRevision: storedRow.WorkflowRevision,
		Goal: storedRow.Goal, SandboxProfile: storedRow.SandboxProfile, ProviderConnection: storedRow.ProviderConnection,
		Model: storedRow.Model, ReasoningEffort: storedRow.ReasoningEffort,
	}}
	expectedCore := input
	expectedCore.Repository, expectedCore.Revision, expectedCore.Branch = "", "", ""
	expectedCore.GitHubRepository, expectedCore.GitHubInstallation, expectedCore.BaseBranch = "", "", ""
	expectedCore.InvestigationSource = investigation.Source{}
	if storedRow.ID != id || stored != expectedCore {
		return core.Job{}, false, fmt.Errorf("admission key %q is already bound to different complete Job input", input.AdmissionKey)
	}
	if input.Workflow == coding.Workflow {
		if _, err := queries.InsertCodingToProposalInput(ctx, dbsql.InsertCodingToProposalInputParams{
			JobID: id, Repository: input.Repository, StartingRevision: input.Revision, Revision: input.Revision,
			Branch: input.Branch, GithubRepository: input.GitHubRepository,
			GithubInstallationID: input.GitHubInstallation, BaseBranch: input.BaseBranch,
		}); err != nil {
			return core.Job{}, false, err
		}
		coding, err := queries.GetCodingToProposalInput(ctx, id)
		if err != nil {
			return core.Job{}, false, err
		}
		stored.Repository, stored.Revision, stored.Branch = coding.Repository, coding.StartingRevision, coding.Branch
		stored.GitHubRepository, stored.GitHubInstallation, stored.BaseBranch = coding.GithubRepository, coding.GithubInstallationID, coding.BaseBranch
	} else {
		if _, err := queries.InsertCodebaseInvestigationSource(ctx, investigationSourceParams(id, input.InvestigationSource)); err != nil {
			return core.Job{}, false, err
		}
		sourceRow, err := queries.GetCodebaseInvestigationSource(ctx, id)
		if err != nil {
			return core.Job{}, false, err
		}
		if sourceRow.JobID != id {
			return core.Job{}, false, fmt.Errorf("codebase-investigation source conflicts with its exact Job")
		}
		stored.InvestigationSource = investigationSourceFromValues("", sourceRow.Kind, sourceRow.Repository, sourceRow.Revision, sourceRow.BundleDigest, sourceRow.BundleByteSize)
	}
	if stored != input {
		return core.Job{}, false, fmt.Errorf("admission key %q is already bound to different complete Job input", input.AdmissionKey)
	}
	messageID := core.MessageID(id, "human", initialFromID)
	if err := queries.InsertInitialMessage(ctx, dbsql.InsertInitialMessageParams{ID: messageID, JobID: id, FromID: initialFromID, Input: input.Goal}); err != nil {
		return core.Job{}, false, err
	}
	initial, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{JobID: id, FromKind: core.MessageFromHuman, FromID: initialFromID})
	if err != nil {
		return core.Job{}, false, err
	}
	if initial.ID != messageID || initial.JobID != id || initial.FromKind != core.MessageFromHuman || initial.FromID != initialFromID ||
		initial.Sequence != 1 || initial.Input != input.Goal || initial.DeliveryIntent != core.MessageFollow || initial.SteerTargetTurnID != "" {
		return core.Job{}, false, fmt.Errorf("Job %s initial message conflicts with complete admission input", id)
	}
	runID := core.AgentRunID(initial.ID)
	sandboxID := core.MainSandboxName(id)
	ownerNonce, err := reviewNonce()
	if err != nil {
		return core.Job{}, false, err
	}
	if err := expectOneRows(queries.ReserveSandbox(ctx, dbsql.ReserveSandboxParams{ID: sandboxID, JobID: id, OwnershipNonce: ownerNonce})); err != nil {
		// A retry must retain the originally reserved ownership nonce.
		if _, getErr := queries.GetSandbox(ctx, sandboxID); getErr != nil {
			return core.Job{}, false, err
		}
	}
	if input.Workflow == investigation.Workflow {
		if _, err := queries.InsertInvestigationAgentRun(ctx, dbsql.InsertInvestigationAgentRunParams{ID: runID, JobID: id, MessageID: initial.ID, InputRevision: nullableString(revision), SandboxID: sandboxID}); err != nil {
			return core.Job{}, false, err
		}
	} else {
		if _, err := queries.InsertImplementationAgentRun(ctx, dbsql.InsertImplementationAgentRunParams{ID: runID, JobID: id, MessageID: initial.ID, SandboxID: sandboxID}); err != nil {
			return core.Job{}, false, err
		}
	}
	if input.Workflow == coding.Workflow {
		if err := queries.InsertInitialRevision(ctx, dbsql.InsertInitialRevisionParams{JobID: id, OID: input.Revision, Branch: input.Branch}); err != nil {
			return core.Job{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return core.Job{}, false, err
	}
	job, err := s.Job(ctx, id)
	return job, rows == 1, err
}

func normalizeInvestigationSource(source investigation.Source) investigation.Source {
	source.JobID = ""
	source.Kind = investigation.SourceKind(strings.TrimSpace(string(source.Kind)))
	source.Repository = strings.TrimSpace(source.Repository)
	source.Revision = strings.TrimSpace(source.Revision)
	source.BundleDigest = strings.TrimSpace(source.BundleDigest)
	return source
}

func validateInvestigationSource(source investigation.Source) error {
	if !ValidRevision(source.Revision) {
		return fmt.Errorf("codebase-investigation source requires a lowercase full commit OID")
	}
	switch source.Kind {
	case investigation.SourceRemote:
		if source.Repository == "" || source.BundleDigest != "" || source.BundleByteSize != 0 {
			return fmt.Errorf("remote investigation source requires only a repository URL and exact Revision")
		}
	case investigation.SourceGitBundle:
		if source.Repository != "" || !sha256Digest.MatchString(source.BundleDigest) || source.BundleByteSize <= 0 {
			return fmt.Errorf("Git-bundle investigation source requires exact retained digest, byte size, and Revision")
		}
	default:
		return fmt.Errorf("codebase-investigation requires a remote or git-bundle source")
	}
	return nil
}

func investigationSourceParams(jobID string, source investigation.Source) dbsql.InsertCodebaseInvestigationSourceParams {
	return dbsql.InsertCodebaseInvestigationSourceParams{
		JobID: jobID, Kind: string(source.Kind), Repository: source.Repository, Revision: source.Revision,
		BundleDigest:   nullableString(source.BundleDigest),
		BundleByteSize: sql.NullInt64{Int64: source.BundleByteSize, Valid: source.BundleByteSize > 0},
	}
}

func investigationSourceFromValues(jobID, kind, repository, revision, digest string, byteSize int64) investigation.Source {
	return investigation.Source{JobID: jobID, Kind: investigation.SourceKind(kind), Repository: repository, Revision: revision, BundleDigest: digest, BundleByteSize: byteSize}
}

type admittedAgentRun struct {
	Role          string
	Capability    string
	InputRevision string
	SandboxID     string
	Harness       string
	ThreadID      string
	TargetTurnID  string
}

type messageAuthorizer func(context.Context, *dbsql.Queries, dbsql.GetJobAdmissionForUpdateRow, core.MessageAdmission) (admittedAgentRun, error)

func (s Store) AdmitCodingMessage(ctx context.Context, input core.MessageAdmission) (core.Message, bool, error) {
	return s.admitMessage(ctx, input, coding.Workflow, coding.WorkflowRevision, authorizeCodingMessage)
}

func (s Store) AdmitInvestigationMessage(ctx context.Context, input core.MessageAdmission) (core.Message, bool, error) {
	return s.admitMessage(ctx, input, investigation.Workflow, investigation.WorkflowRevision, authorizeInvestigationMessage)
}

func (s Store) admitMessage(ctx context.Context, input core.MessageAdmission, workflow core.WorkflowName, revision string, authorize messageAuthorizer) (core.Message, bool, error) {
	input, err := normalizeMessage(input)
	if err != nil {
		return core.Message{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.Message{}, false, err
	}
	defer tx.Rollback()
	message, created, err := admitMessageTx(ctx, tx, input, workflow, revision, authorize)
	if err != nil {
		return core.Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return core.Message{}, false, err
	}
	return message, created, nil
}

func normalizeMessage(input core.MessageAdmission) (core.MessageAdmission, error) {
	input.JobID = strings.TrimSpace(input.JobID)
	input.FromKind = core.MessageFromKind(strings.TrimSpace(string(input.FromKind)))
	input.FromID = strings.TrimSpace(input.FromID)
	if input.FromKind == "" {
		input.FromKind = core.MessageFromHuman
	}
	if input.Intent == "" {
		input.Intent = core.MessageFollow
	}
	if input.JobID == "" || input.FromID == "" || strings.TrimSpace(input.Input) == "" {
		return core.MessageAdmission{}, fmt.Errorf("message admission requires Job ID, from ID, and complete input")
	}
	if input.FromKind != core.MessageFromHuman && input.FromKind != core.MessageFromAgent && input.FromKind != core.MessageFromWorkflow {
		return core.MessageAdmission{}, fmt.Errorf("invalid message from kind")
	}
	if len(input.FromID) > 256 {
		return core.MessageAdmission{}, fmt.Errorf("from ID must be at most 256 characters")
	}
	if input.Intent != core.MessageFollow && input.Intent != core.MessageSteer {
		return core.MessageAdmission{}, fmt.Errorf("message intent must be follow or steer")
	}
	if len(input.Input) > 1<<20 {
		return core.MessageAdmission{}, fmt.Errorf("message input exceeds 1 MiB")
	}
	return input, nil
}

func admitMessageTx(ctx context.Context, tx *sql.Tx, input core.MessageAdmission, workflow core.WorkflowName, revision string, authorize messageAuthorizer) (core.Message, bool, error) {
	queries := dbsql.New(tx)
	job, err := queries.GetJobAdmissionForUpdate(ctx, input.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return core.Message{}, false, ErrNotFound
		}
		return core.Message{}, false, err
	}
	if job.WorkflowName != workflow || job.WorkflowRevision != revision {
		return core.Message{}, false, fmt.Errorf("Job %s is not %s revision %s", input.JobID, workflow, revision)
	}
	row, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{JobID: input.JobID, FromKind: input.FromKind, FromID: input.FromID})
	if err == nil {
		message := messageFromValues(row.ID, row.JobID, row.FromKind, row.FromID, row.Sequence, row.Input, row.DeliveryIntent, row.SteerTargetTurnID)
		message.AdmittedAt = row.AdmittedAt
		if message.Input != input.Input || message.Intent != input.Intent {
			return core.Message{}, false, fmt.Errorf("sender %s/%q is already bound to different complete message input or delivery intent", input.FromKind, input.FromID)
		}
		return message, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return core.Message{}, false, err
	}
	if !job.AdmissionOpen {
		return core.Message{}, false, fmt.Errorf("Job %s admission is closed for cleanup", input.JobID)
	}
	if authorize == nil {
		return core.Message{}, false, fmt.Errorf("message admission policy is not configured")
	}
	run, err := authorize(ctx, queries, job, input)
	if err != nil {
		return core.Message{}, false, err
	}
	var message core.Message
	message.TargetTurnID = run.TargetTurnID
	message.Sequence, err = queries.NextMessageSequence(ctx, input.JobID)
	if err != nil {
		return core.Message{}, false, err
	}
	message.ID = core.MessageID(input.JobID, input.FromKind, input.FromID)
	message.JobID, message.FromKind, message.FromID, message.Input, message.Intent = input.JobID, input.FromKind, input.FromID, input.Input, input.Intent
	if err := queries.InsertMessage(ctx, dbsql.InsertMessageParams{ID: message.ID, JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID, Sequence: message.Sequence, Input: message.Input, DeliveryIntent: message.Intent, SteerTargetTurnID: message.TargetTurnID}); err != nil {
		return core.Message{}, false, err
	}
	runID := core.AgentRunID(message.ID)
	rows, err := queries.InsertAdmittedAgentRun(ctx, dbsql.InsertAdmittedAgentRunParams{
		ID: runID, JobID: message.JobID, MessageID: message.ID,
		Harness: nullableString(run.Harness), ThreadID: nullableString(run.ThreadID),
		Role: run.Role, InputRevision: nullableString(run.InputRevision),
		Capability: nullableString(run.Capability), SandboxID: run.SandboxID,
	})
	if err := expectOneRows(rows, err); err != nil {
		return core.Message{}, false, fmt.Errorf("insert authorized %s AgentRun: %w", run.Role, err)
	}
	storedMessage, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{JobID: message.JobID, FromKind: message.FromKind, FromID: message.FromID})
	if err != nil {
		return core.Message{}, false, err
	}
	message.AdmittedAt = storedMessage.AdmittedAt
	return message, true, nil
}

func allocateMessageSequenceTx(ctx context.Context, tx *sql.Tx, jobID string) (int64, error) {
	return dbsql.New(tx).NextMessageSequence(ctx, jobID)
}

func ensureInputsTerminalForWorkflowTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	row, err := dbsql.New(tx).GetFirstUnsettledInput(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	state := string(row.State)
	if row.Attention != "" {
		state += ": " + row.Attention
	}
	return fmt.Errorf("FIFO sequence %d has not reached a terminal harness delivery (%s)", row.Sequence, state)
}

func (s Store) Job(ctx context.Context, id string) (core.Job, error) {
	row, err := dbsql.New(s.DB).GetJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Job{}, ErrNotFound
	}
	if err != nil {
		return core.Job{}, err
	}
	return core.Job{
		ID: row.ID, AdmissionKey: row.AdmissionKey, Workflow: core.WorkflowName(row.WorkflowName), WorkflowRevision: row.WorkflowRevision,
		Goal:           row.Goal,
		SandboxProfile: row.SandboxProfile, ProviderConnection: row.ProviderConnection,
		Model: row.Model, ReasoningEffort: row.ReasoningEffort, AdmissionOpen: row.AdmissionOpen, CleanupState: core.CleanupState(row.CleanupState),
		CurrentTaskID:     row.CurrentTaskID,
		WorkflowAttention: row.WorkflowAttention, WorkflowAttentionSource: row.WorkflowAttentionSource,
		WorkflowAttentionAt: timeValue(row.WorkflowAttentionAt), CleanupAttention: row.CleanupAttention,
		AdmittedAt: row.AdmittedAt, CleanedAt: timeValue(row.CleanedAt),
	}, nil
}

func (s Store) JobExists(ctx context.Context, id string) (bool, error) {
	_, err := s.Job(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s Store) CodingJob(ctx context.Context, id string) (coding.Job, error) {
	row, err := dbsql.New(s.DB).GetCodingJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return coding.Job{}, ErrNotFound
	}
	if err != nil {
		return coding.Job{}, err
	}
	return coding.Job{
		Job: core.Job{
			ID: row.ID, AdmissionKey: row.AdmissionKey, Workflow: core.WorkflowName(row.WorkflowName), WorkflowRevision: row.WorkflowRevision,
			Goal: row.Goal, SandboxProfile: row.SandboxProfile, ProviderConnection: row.ProviderConnection,
			Model: row.Model, ReasoningEffort: row.ReasoningEffort, AdmissionOpen: row.AdmissionOpen, CleanupState: core.CleanupState(row.CleanupState),
			CurrentTaskID: row.CurrentTaskID, WorkflowAttention: row.WorkflowAttention, WorkflowAttentionSource: row.WorkflowAttentionSource,
			WorkflowAttentionAt: timeValue(row.WorkflowAttentionAt), CleanupAttention: row.CleanupAttention,
			AdmittedAt: row.AdmittedAt, CleanedAt: timeValue(row.CleanedAt),
		},
		Repository: row.Repository, StartingRevision: row.StartingRevision, Revision: row.Revision, Branch: row.Branch,
		GitHubRepository: row.GithubRepository, GitHubInstallation: row.GithubInstallationID, BaseBranch: row.BaseBranch,
	}, nil
}

func (s Store) JobTasks(ctx context.Context, jobID string) ([]core.JobTask, error) {
	rows, err := dbsql.New(s.DB).ListJobTasks(ctx, jobID)
	if err != nil {
		return nil, err
	}
	tasks := make([]core.JobTask, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, core.JobTask{
			JobID: row.JobID, Sequence: row.Sequence, TaskID: row.TaskID,
			TaskName: row.TaskName, AttachedAt: row.AttachedAt,
		})
	}
	return tasks, nil
}

func (s Store) Revisions(ctx context.Context, jobID string) ([]coding.Revision, error) {
	rows, err := dbsql.New(s.DB).ListRevisions(ctx, jobID)
	if err != nil {
		return nil, err
	}
	revisions := make([]coding.Revision, 0, len(rows))
	for _, row := range rows {
		revisions = append(revisions, coding.Revision{
			JobID: row.JobID, OID: row.OID, ComparisonBase: row.ComparisonBaseOID,
			Tree: row.TreeOID, Branch: row.Branch, Generation: int(row.Generation),
			EvidenceID: row.EvidenceID, ObservedAt: row.ObservedAt,
		})
	}
	return revisions, nil
}

// WithJobFence serializes harness and other external mutation for one Job
// independently of an expiring Absurd claim. Message admission intentionally
// does not take this long-lived fence.
func (s Store) WithJobFence(ctx context.Context, jobID string, fn func() error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := acquireJobFenceTx(ctx, tx, jobID); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return tx.Commit()
}

func acquireJobFenceTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(hashtextextended('dorf-job-effect:' || $1, 0))`, jobID); err != nil {
		return fmt.Errorf("acquire Job execution fence: %w", err)
	}
	return nil
}

// AttachJobTask appends one exact Absurd task handoff. The deterministic Absurd
// idempotency key supplies task identity; Dorf records only ordered attachment.
func (s Store) AttachJobTask(ctx context.Context, jobID, expectedCurrentTaskID, taskID, taskName string) error {
	return s.attachJobTask(ctx, jobID, expectedCurrentTaskID, taskID, taskName, false)
}

func messageFromValues(id, jobID string, fromKind core.MessageFromKind, fromID string, sequence int64, input string, intent core.MessageDeliveryIntent, targetTurnID string) core.Message {
	return core.Message{ID: id, JobID: jobID, FromKind: fromKind, FromID: fromID, Sequence: sequence, Input: input, Intent: intent, TargetTurnID: targetTurnID}
}

func actionFromValues(id, jobID string, kind core.ActionKind, state core.ActionState, scope string, createdAt time.Time, settledAt sql.NullTime) core.Action {
	return core.Action{ID: id, JobID: jobID, Kind: kind, State: state, Scope: scope, CreatedAt: createdAt, SettledAt: timeValue(settledAt)}
}

func exactScopedAction(row dbsql.DorfAction, jobID string, kind core.ActionKind, scope string) (core.Action, error) {
	expectedID := core.ScopedActionID(jobID, kind, scope)
	if row.ID != expectedID || row.JobID != jobID || row.Kind != kind || row.ScopeKey != scope {
		return core.Action{}, fmt.Errorf("Action %s conflicts with exact Job %s, kind %s, and scope %s", row.ID, jobID, kind, scope)
	}
	return actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt), nil
}

func agentRunFromValues(id, jobID, messageID string, state core.AgentRunState, harness, threadID string, baselineRecorded bool, baselineTurnID, turnID, turnOutcome, attention, role, inputRevision string) core.AgentRun {
	return core.AgentRun{ID: id, JobID: jobID, MessageID: messageID, Harness: harness, ThreadID: threadID, State: state, BaselineRecorded: baselineRecorded, BaselineTurnID: baselineTurnID, TurnID: turnID, TurnOutcome: turnOutcome, Attention: attention, Role: role, InputRevision: inputRevision}
}

func timeValue(value sql.NullTime) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return value.Time
}

func (s Store) CloseAdmissionForCleanup(ctx context.Context, jobID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	if _, err := queries.GetCleanupJobForUpdate(ctx, jobID); err != nil {
		return err
	}
	closed, err := queries.CloseAdmissionForCleanup(ctx, jobID)
	if err != nil {
		return err
	}
	if closed != 1 {
		return fmt.Errorf("cleanup cannot close admission while a pull-request Action or Proposal lacks a recorded Outcome")
	}
	return tx.Commit()
}

func (s Store) AttachCleanupTask(ctx context.Context, jobID, expectedCurrentTaskID, taskID, taskName string) error {
	return s.attachJobTask(ctx, jobID, expectedCurrentTaskID, taskID, taskName, true)
}

func (s Store) attachJobTask(ctx context.Context, jobID, expectedCurrentTaskID, taskID, taskName string, cleanup bool) error {
	jobID = strings.TrimSpace(jobID)
	expectedCurrentTaskID = strings.TrimSpace(expectedCurrentTaskID)
	taskID = strings.TrimSpace(taskID)
	taskName = strings.TrimSpace(taskName)
	if jobID == "" || taskID == "" || taskName == "" {
		return fmt.Errorf("Job task attachment requires exact Job, task, and task-name identities")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	current, err := queries.GetCurrentJobTaskForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if current.TaskID == taskID {
		if current.TaskName != taskName {
			return fmt.Errorf("Absurd task %s is already attached as %s", taskID, current.TaskName)
		}
	} else {
		if current.TaskID != expectedCurrentTaskID {
			return fmt.Errorf("Job %s current task is %q, not expected predecessor %q", jobID, current.TaskID, expectedCurrentTaskID)
		}
		inserted, err := queries.InsertJobTask(ctx, dbsql.InsertJobTaskParams{
			JobID: jobID, Sequence: current.Sequence + 1, TaskID: taskID, TaskName: taskName,
		})
		if err != nil {
			return err
		}
		if inserted != 1 {
			return fmt.Errorf("Absurd task %s is already attached to another Job", taskID)
		}
	}
	if cleanup {
		updated, err := queries.MarkCleanupScheduled(ctx, jobID)
		if err != nil {
			return err
		}
		if updated != 1 {
			return fmt.Errorf("Job %s cleanup scheduling did not settle", jobID)
		}
	}
	return tx.Commit()
}

func (s Store) GetOrCreateSandboxAction(ctx context.Context, sandboxID string, kind core.ActionKind) (core.Action, error) {
	sandbox, err := dbsql.New(s.DB).GetSandbox(ctx, sandboxID)
	if err != nil {
		return core.Action{}, err
	}
	id := core.ScopedActionID(sandbox.JobID, kind, sandboxID)
	q := dbsql.New(s.DB)
	insertErr := expectOneRows(q.InsertScopedAction(ctx, dbsql.InsertScopedActionParams{ID: id, JobID: sandbox.JobID, Kind: kind, ScopeKey: sandboxID}))
	row, getErr := q.GetScopedAction(ctx, dbsql.GetScopedActionParams{JobID: sandbox.JobID, Kind: kind, ScopeKey: sandboxID})
	if getErr != nil {
		if insertErr != nil {
			return core.Action{}, insertErr
		}
		return core.Action{}, getErr
	}
	return exactScopedAction(row, sandbox.JobID, kind, sandboxID)
}

func (s Store) Sandbox(ctx context.Context, id string) (core.Sandbox, error) {
	row, err := dbsql.New(s.DB).GetSandbox(ctx, id)
	if err != nil {
		return core.Sandbox{}, err
	}
	return core.Sandbox{ID: row.ID, JobID: row.JobID, OwnershipNonce: row.OwnershipNonce}, nil
}
func (s Store) Sandboxes(ctx context.Context, jobID string) ([]core.Sandbox, error) {
	rows, err := dbsql.New(s.DB).ListJobSandboxes(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Sandbox, 0, len(rows))
	for _, r := range rows {
		out = append(out, core.Sandbox{ID: r.ID, JobID: r.JobID, OwnershipNonce: r.OwnershipNonce})
	}
	return out, nil
}

func (s Store) Deliveries(ctx context.Context, jobID string) ([]core.Delivery, error) {
	rows, err := dbsql.New(s.DB).ListDeliveries(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Delivery, 0, len(rows))
	for _, r := range rows {
		if !r.AgentRunPresent {
			return nil, fmt.Errorf("Message %s (sequence %d) has no AgentRun", r.MessageID, r.Sequence)
		}
		if r.AgentRunMessageID != r.MessageID || r.AgentRunJobID != r.MessageJobID {
			return nil, fmt.Errorf("Message %s (Job %s) has mismatched AgentRun %s (Message %s, Job %s)", r.MessageID, r.MessageJobID, r.AgentRunID, r.AgentRunMessageID, r.AgentRunJobID)
		}
		message := messageFromValues(r.MessageID, r.MessageJobID, r.FromKind, r.FromID, r.Sequence, r.Input, r.DeliveryIntent, r.SteerTargetTurnID)
		message.AdmittedAt = r.AdmittedAt
		run := agentRunFromValues(r.AgentRunID, r.AgentRunJobID, r.AgentRunMessageID, r.State, r.Harness, r.ThreadID, r.BaselineRecorded, r.BaselineTurnID, r.TurnID, r.TurnOutcome, r.Attention, r.Role, r.InputRevision)
		run.Capability = r.Capability
		run.SandboxID = r.SandboxID
		run.SubmissionNonce = r.SubmissionNonce
		run.StartedAt = timeValue(r.StartedAt)
		run.FinishedAt = timeValue(r.FinishedAt)
		out = append(out, core.Delivery{Message: message, AgentRun: run})
	}
	return out, nil
}

func (s Store) InterruptAgentRun(ctx context.Context, runID, reason string) error {
	q := dbsql.New(s.DB)
	row, err := q.GetAgentRunForBinding(ctx, runID)
	if err != nil {
		return err
	}
	if row.State == core.AgentRunCompleted || row.State == core.AgentRunFailed || row.State == core.AgentRunInterrupted {
		return nil
	}
	return expectOneRows(q.InterruptAgentRun(ctx, dbsql.InterruptAgentRunParams{Reason: reason, RunID: runID}))
}

func revisionCandidateTx(ctx context.Context, tx *sql.Tx, jobID string) (core.AgentRun, bool, error) {
	queries := dbsql.New(tx)
	unsettled, err := queries.CountUnsettledInputs(ctx, jobID)
	if err != nil {
		return core.AgentRun{}, false, err
	}
	if unsettled != 0 {
		return core.AgentRun{}, false, nil
	}
	latestInput, err := queries.GetLatestImplementationRun(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AgentRun{}, false, nil
	}
	if err != nil {
		return core.AgentRun{}, false, err
	}
	if latestInput.State != core.AgentRunCompleted {
		return core.AgentRun{}, false, nil
	}
	row, err := queries.GetLatestTurnStartRun(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AgentRun{}, false, nil
	}
	if err != nil {
		return core.AgentRun{}, false, err
	}
	run := core.AgentRun{ID: row.ID, JobID: row.JobID, State: row.State, Role: row.Role, InputRevision: row.InputRevision}
	if row.Observed {
		return core.AgentRun{}, false, nil
	}
	if run.State != core.AgentRunCompleted || run.Role != "implement" {
		return core.AgentRun{}, false, nil
	}
	return run, true, nil
}

func insertEvidence(ctx context.Context, tx *sql.Tx, jobID string, evidence core.Evidence) error {
	queries := dbsql.New(tx)
	err := queries.InsertEvidence(ctx, dbsql.InsertEvidenceParams{
		ID: evidence.ID, JobID: jobID, Digest: evidence.Digest, ByteSize: evidence.ByteSize,
		MediaType: evidence.MediaType, Producer: evidence.Producer,
		Kind: evidence.Kind, ActionID: evidence.ActionID, AgentRunID: evidence.AgentRunID, Revision: evidence.Revision,
		StartedAt: nullableTime(evidence.StartedAt), FinishedAt: nullableTime(evidence.FinishedAt),
	})
	if err != nil {
		return err
	}
	stored, err := queries.GetEvidenceIdentity(ctx, evidence.ID)
	if err != nil {
		return err
	}
	if stored.JobID != jobID || stored.Digest != evidence.Digest || stored.ByteSize != evidence.ByteSize || stored.MediaType != evidence.MediaType || stored.Producer != evidence.Producer || stored.Kind != evidence.Kind || stored.ActionID != evidence.ActionID || stored.AgentRunID != evidence.AgentRunID || stored.Revision != evidence.Revision || !stored.StartedAt.Equal(evidence.StartedAt) || !stored.FinishedAt.Equal(evidence.FinishedAt) {
		return fmt.Errorf("Evidence identity %s conflicts with immutable retained metadata or content", evidence.ID)
	}
	return nil
}

func nullableTime(value time.Time) sql.NullTime {
	if value.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: value, Valid: true}
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func (s Store) SetWorkflowAttention(ctx context.Context, jobID, source, detail string) error {
	source, detail = strings.TrimSpace(source), strings.TrimSpace(detail)
	if jobID == "" || source == "" || detail == "" {
		return fmt.Errorf("workflow attention requires Job ID, exact source, and detail")
	}
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return expectOneRows(dbsql.New(s.DB).SetWorkflowAttention(ctx, dbsql.SetWorkflowAttentionParams{JobID: jobID, Source: sql.NullString{String: source, Valid: true}, Detail: sql.NullString{String: detail, Valid: true}}))
}

func (s Store) RecordRevisionObservation(ctx context.Context, jobID, runID string, observation gitworkspace.Observation, evidence core.Evidence) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetRevisionJobForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if evidence.ID != core.EvidenceID(runID, "git-revision") || evidence.ActionID != "" || evidence.AgentRunID != runID || evidence.Kind != "git-revision" || evidence.Revision != observation.Revision ||
		!ValidRevision(observation.ComparisonBase) || !ValidRevision(observation.Revision) || !ValidRevision(observation.Tree) {
		return fmt.Errorf("Git Revision observation conflicts with durable comparison base, branch, AgentRun, or Evidence")
	}
	if _, err := queries.GetEvidenceIdentity(ctx, evidence.ID); err == nil {
		if err := insertEvidence(ctx, tx, jobID, evidence); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !locked.AdmissionOpen || locked.OutcomeExists {
		return fmt.Errorf("%w: admission is closed or the Job has an Outcome", ErrRevisionObservationSuperseded)
	}
	if locked.Revision != observation.ComparisonBase {
		return fmt.Errorf("%w: comparison base %s is not current Revision %s", ErrRevisionObservationSuperseded, observation.ComparisonBase, locked.Revision)
	}
	if locked.Branch != observation.Branch {
		return fmt.Errorf("Git Revision observation branch %s conflicts with Job branch %s", observation.Branch, locked.Branch)
	}
	candidate, ready, err := revisionCandidateTx(ctx, tx, jobID)
	if err != nil {
		return err
	}
	if !ready || candidate.ID != runID || candidate.InputRevision != observation.ComparisonBase {
		return fmt.Errorf("%w: AgentRun %s no longer owns the latest completed implementation turn", ErrRevisionObservationSuperseded, runID)
	}
	if err := insertEvidence(ctx, tx, jobID, evidence); err != nil {
		return err
	}
	if observation.Revision != observation.ComparisonBase {
		generation, err := queries.NextRevisionGeneration(ctx, jobID)
		if err != nil {
			return err
		}
		if err := queries.InsertRevision(ctx, dbsql.InsertRevisionParams{
			JobID: jobID, OID: observation.Revision, ComparisonBaseOID: observation.ComparisonBase,
			TreeOID: observation.Tree, Branch: observation.Branch, Generation: generation, EvidenceID: evidence.ID,
		}); err != nil {
			return err
		}
		updated, err := queries.AdvanceJobRevision(ctx, dbsql.AdvanceJobRevisionParams{JobID: jobID, Revision: observation.Revision, ComparisonBaseOID: observation.ComparisonBase})
		if err != nil {
			return err
		}
		if updated != 1 {
			return ErrNotFound
		}
	}
	if _, err := queries.ClearWorkflowAttention(ctx, dbsql.ClearWorkflowAttentionParams{JobID: jobID, Source: sql.NullString{String: runID, Valid: true}}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s Store) RecordSandboxActionSuccess(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	completed, err := queries.GetActionByIDForUpdate(ctx, id)
	if err != nil {
		return err
	}
	if completed.State != core.ActionUnsettled && completed.State != core.ActionSucceeded {
		return fmt.Errorf("Sandbox Action %s is %s, not unsettled or succeeded", id, completed.State)
	}
	kind, scope := completed.Kind, completed.ScopeKey
	if scope == "" {
		return fmt.Errorf("Sandbox Action %s has no exact Sandbox", id)
	}
	sandbox, err := queries.GetSandbox(ctx, scope)
	if err != nil {
		return err
	}
	if completed.ID != id || sandbox.JobID != completed.JobID || id != core.ScopedActionID(completed.JobID, kind, scope) {
		return fmt.Errorf("Sandbox Action %s conflicts with its exact Job and Sandbox", id)
	}
	if kind == core.ActionSandboxDelete {
		revokedRow, getErr := queries.GetScopedAction(ctx, dbsql.GetScopedActionParams{JobID: completed.JobID, Kind: core.ActionRouteRevoke, ScopeKey: scope})
		if errors.Is(getErr, sql.ErrNoRows) {
			err = fmt.Errorf("Sandbox cleanup cannot complete before its exact route revoke Action succeeds")
		} else if getErr != nil {
			err = getErr
		} else {
			var revoked core.Action
			revoked, err = exactScopedAction(revokedRow, completed.JobID, core.ActionRouteRevoke, scope)
			if err == nil && revoked.State != core.ActionSucceeded {
				err = fmt.Errorf("Sandbox cleanup cannot complete before its exact route revoke Action succeeds")
			}
		}
	}
	if err != nil {
		return err
	}
	if completed.State == core.ActionSucceeded {
		return tx.Commit()
	}
	if err := expectOneRows(queries.RecordSandboxActionSuccess(ctx, id)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) NextDelivery(ctx context.Context, jobID string) (*core.Delivery, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	job, err := queries.GetRevisionJobForUpdate(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if !job.AdmissionOpen || job.OutcomeExists {
		return nil, nil
	}
	// A steer is a distinct priority lane aimed at the active harness Turn. It may
	// overtake older queued follow-ups; the immutable sequence still records
	// admission order, while follow-up turn starts remain FIFO.
	row, err := queries.NextDeliveryCandidate(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	message := core.Message{
		ID: row.ID, JobID: row.JobID, FromKind: core.MessageFromKind(row.FromKind), FromID: row.FromID,
		Sequence: row.Sequence, Input: row.Input, Intent: core.MessageDeliveryIntent(row.DeliveryIntent), TargetTurnID: row.SteerTargetTurnID, AdmittedAt: row.AdmittedAt,
	}
	runID := core.AgentRunID(message.ID)
	if _, err := queries.InsertImplementationAgentRun(ctx, dbsql.InsertImplementationAgentRunParams{ID: runID, JobID: jobID, MessageID: message.ID, SandboxID: core.MainSandboxName(jobID)}); err != nil {
		return nil, err
	}
	runRow, err := queries.GetAgentRunByMessage(ctx, message.ID)
	if err != nil {
		return nil, err
	}
	run := agentRunFromValues(runRow.ID, runRow.JobID, runRow.MessageID, runRow.State, runRow.Harness, runRow.ThreadID, runRow.BaselineRecorded, runRow.BaselineTurnID, runRow.TurnID, runRow.TurnOutcome, runRow.Attention, runRow.Role, runRow.InputRevision)
	run.SandboxID = runRow.SandboxID
	if run.Role != "implement" {
		return nil, fmt.Errorf("delivery candidate AgentRun %s has unsupported role %s", run.ID, run.Role)
	}
	if run.InputRevision == "" {
		if err := expectOneRows(queries.BindImplementationInputRevision(ctx, dbsql.BindImplementationInputRevisionParams{InputRevision: sql.NullString{String: job.Revision, Valid: true}, RunID: run.ID})); err != nil {
			return nil, err
		}
		run.InputRevision = job.Revision
	} else if run.InputRevision != job.Revision && run.State == core.AgentRunPending {
		return nil, fmt.Errorf("AgentRun %s input Revision %s conflicts with current Revision %s", run.ID, run.InputRevision, job.Revision)
	}
	bindings, err := queries.ListImplementationThreadBindings(ctx, jobID)
	if err != nil {
		return nil, err
	}
	for i, binding := range bindings {
		if i > 0 && (binding.Harness != bindings[0].Harness || binding.ThreadID != bindings[0].ThreadID) ||
			run.ThreadID != "" && (run.Harness != binding.Harness.String || run.ThreadID != binding.ThreadID.String) {
			return nil, fmt.Errorf("Job %s implementation AgentRuns disagree on their harness Thread", jobID)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &core.Delivery{Message: message, AgentRun: run}, nil
}

// DeliveryCandidate exposes the same FIFO/steer choice as NextDelivery without
// binding the AgentRun or mutating any workflow fact.
func (s Store) DeliveryCandidate(ctx context.Context, jobID string) (*core.Delivery, error) {
	queries := dbsql.New(s.DB)
	row, err := queries.NextDeliveryCandidate(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	message := core.Message{
		ID: row.ID, JobID: row.JobID, FromKind: core.MessageFromKind(row.FromKind), FromID: row.FromID,
		Sequence: row.Sequence, Input: row.Input, Intent: core.MessageDeliveryIntent(row.DeliveryIntent), TargetTurnID: row.SteerTargetTurnID, AdmittedAt: row.AdmittedAt,
	}
	runRow, err := queries.GetAgentRunByMessage(ctx, message.ID)
	if err != nil {
		return nil, err
	}
	run := agentRunFromValues(runRow.ID, runRow.JobID, runRow.MessageID, runRow.State, runRow.Harness, runRow.ThreadID, runRow.BaselineRecorded, runRow.BaselineTurnID, runRow.TurnID, runRow.TurnOutcome, runRow.Attention, runRow.Role, runRow.InputRevision)
	run.SandboxID = runRow.SandboxID
	return &core.Delivery{Message: message, AgentRun: run}, nil
}

func (s Store) PrepareAgentRun(ctx context.Context, runID, harness, baselineTurnID string) error {
	if strings.TrimSpace(harness) == "" {
		return fmt.Errorf("AgentRun preparation requires a harness")
	}
	queries := dbsql.New(s.DB)
	rows, err := queries.PrepareAgentRun(ctx, dbsql.PrepareAgentRunParams{Harness: sql.NullString{String: harness, Valid: true}, BaselineTurnID: sql.NullString{String: baselineTurnID, Valid: true}, RunID: runID})
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	prepared, err := queries.GetAgentRunPreparation(ctx, runID)
	if err != nil {
		return err
	}
	if prepared.Harness != harness || !prepared.Recorded || prepared.BaselineTurnID != baselineTurnID {
		return fmt.Errorf("AgentRun %s harness baseline conflicts with durable baseline", runID)
	}
	return nil
}

func (s Store) BindAgentRun(ctx context.Context, runID, harness, threadID, turnID, status string) error {
	if strings.TrimSpace(harness) == "" || strings.TrimSpace(threadID) == "" || strings.TrimSpace(turnID) == "" {
		return fmt.Errorf("AgentRun binding requires harness, Thread ID, and Turn ID")
	}
	state := core.AgentRunActive
	outcome := ""
	attention := ""
	if status == "completed" {
		state, outcome = core.AgentRunCompleted, status
	} else if status == "failed" {
		state, outcome = core.AgentRunFailed, status
	} else if status == "interrupted" {
		state, outcome = core.AgentRunInterrupted, status
	} else if status != "running" && status != "inProgress" {
		state = core.AgentRunUncertain
		attention = fmt.Sprintf("harness Turn %s has unsupported status %q", turnID, status)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	run, err := queries.GetAgentRunForBinding(ctx, runID)
	if err != nil {
		return err
	}
	if run.Harness != "" && run.Harness != harness || run.ThreadID != "" && run.ThreadID != threadID || run.TurnID != "" && run.TurnID != turnID {
		return fmt.Errorf("AgentRun %s harness Thread/Turn binding conflicts with its durable identity", runID)
	}
	if run.State == core.AgentRunCompleted || run.State == core.AgentRunFailed || run.State == core.AgentRunInterrupted {
		if run.State != state || run.TurnOutcome != outcome || run.Harness == "" || run.ThreadID == "" || run.TurnID == "" {
			return fmt.Errorf("AgentRun %s terminal outcome conflicts with observed harness status %q", runID, status)
		}
		return tx.Commit()
	}
	if run.State == core.AgentRunPending {
		return fmt.Errorf("AgentRun %s must be prepared before binding a harness Turn", runID)
	}
	if run.Role == "implement" {
		bindings, err := queries.ListImplementationThreadBindings(ctx, run.JobID)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			if binding.Harness.String != harness || binding.ThreadID.String != threadID {
				return fmt.Errorf("AgentRun %s conflicts with Job %s implementation Thread", runID, run.JobID)
			}
		}
	} else {
		inherited, err := queries.ImplementationThreadExists(ctx, dbsql.ImplementationThreadExistsParams{Harness: sql.NullString{String: harness, Valid: true}, ThreadID: sql.NullString{String: threadID, Valid: true}})
		if err != nil {
			return err
		}
		if inherited {
			return fmt.Errorf("review AgentRun %s cannot inherit an implementation Thread", runID)
		}
	}
	if err := expectOneRows(queries.BindAgentRunIdentity(ctx, dbsql.BindAgentRunIdentityParams{Harness: sql.NullString{String: harness, Valid: true}, ThreadID: sql.NullString{String: threadID, Valid: true}, RunID: runID})); err != nil {
		return err
	}
	if err := expectOneRows(queries.BindHarnessTurn(ctx, dbsql.BindHarnessTurnParams{TurnID: sql.NullString{String: turnID, Valid: true}, State: state, TurnOutcome: outcome, Attention: attention, RunID: runID, Harness: sql.NullString{String: harness, Valid: true}, ThreadID: sql.NullString{String: threadID, Valid: true}})); err != nil {
		return err
	}
	if outcome != "" {
		if err := queries.PropagateTurnOutcomeToSteers(ctx, dbsql.PropagateTurnOutcomeToSteersParams{TurnOutcome: sql.NullString{String: outcome, Valid: true}, RunID: runID, TurnID: sql.NullString{String: turnID, Valid: true}}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s Store) BindSteer(ctx context.Context, runID, turnID, status string) error {
	outcome := ""
	if status == "completed" || status == "failed" || status == "interrupted" {
		outcome = status
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	bound, err := queries.BindSteer(ctx, dbsql.BindSteerParams{TurnID: sql.NullString{String: turnID, Valid: true}, TurnOutcome: outcome, RunID: runID})
	if err != nil {
		return err
	}
	if outcome != "" && bound != outcome {
		return fmt.Errorf("AgentRun %s outcome %s conflicts with observed %s", runID, bound, outcome)
	}
	return tx.Commit()
}

func (s Store) FailAgentRun(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).FailAgentRun(ctx, dbsql.FailAgentRunParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) UncertainAgentRun(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).MarkAgentRunUncertain(ctx, dbsql.MarkAgentRunUncertainParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) AgentRunAttention(ctx context.Context, runID, reason string) error {
	return expectOneRows(dbsql.New(s.DB).SetAgentRunAttention(ctx, dbsql.SetAgentRunAttentionParams{Reason: sql.NullString{String: reason, Valid: true}, RunID: runID}))
}

func (s Store) HarnessMutationDelivery(ctx context.Context, jobID string) (*core.Delivery, error) {
	row, err := dbsql.New(s.DB).GetHarnessMutationDelivery(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	delivery := core.Delivery{
		Message:  messageFromValues(row.MessageID, row.JobID, row.FromKind, row.FromID, row.Sequence, row.Input, row.DeliveryIntent, row.SteerTargetTurnID),
		AgentRun: agentRunFromValues(row.AgentRunID, row.AgentRunJobID, row.AgentRunMessageID, row.State, row.Harness, row.ThreadID, row.BaselineRecorded, row.BaselineTurnID, row.TurnID, row.TurnOutcome, row.Attention, row.Role, row.InputRevision),
	}
	delivery.Message.AdmittedAt = row.AdmittedAt
	return &delivery, nil
}

func (s Store) SetCleanupAttention(ctx context.Context, jobID, detail string) error {
	detail = strings.TrimSpace(detail)
	if len(detail) > 4096 {
		detail = detail[:4096]
	}
	return expectOneRows(dbsql.New(s.DB).SetCleanupAttention(ctx, dbsql.SetCleanupAttentionParams{Detail: detail, JobID: jobID}))
}

func (s Store) CompleteCleanup(ctx context.Context, jobID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	job, err := queries.GetCleanupJobForUpdate(ctx, jobID)
	if err != nil {
		return err
	}
	if job.AdmissionOpen || job.CleanupState != core.CleanupScheduled {
		return fmt.Errorf("cleanup cannot complete while admission or cleanup scheduling remains unsettled")
	}
	if job.CurrentTaskID == "" {
		return fmt.Errorf("cleanup cannot complete without the Job's current attached execution task")
	}
	deliveries, err := queries.ListDeliveries(ctx, jobID)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if !delivery.AgentRunPresent {
			return fmt.Errorf("cleanup cannot complete because Message %s has no AgentRun", delivery.MessageID)
		}
		if delivery.AgentRunMessageID != delivery.MessageID || delivery.AgentRunJobID != delivery.MessageJobID {
			return fmt.Errorf("cleanup cannot complete because Message %s has a mismatched AgentRun %s", delivery.MessageID, delivery.AgentRunID)
		}
		run := delivery
		if run.State != core.AgentRunCompleted && run.State != core.AgentRunFailed && run.State != core.AgentRunInterrupted {
			return fmt.Errorf("cleanup cannot complete with unsettled AgentRun %s", run.AgentRunID)
		}
	}
	unsettled, err := queries.CountUnsettledSandboxCleanupActions(ctx, jobID)
	if err != nil {
		return err
	}
	if unsettled != 0 {
		return fmt.Errorf("cleanup cannot complete with %d unsettled Job resources", unsettled)
	}
	if err := expectOneRows(queries.CompleteCleanup(ctx, jobID)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) Actions(ctx context.Context, jobID string) ([]core.Action, error) {
	rows, err := dbsql.New(s.DB).ListActions(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var actions []core.Action
	for _, row := range rows {
		actions = append(actions, actionFromValues(row.ID, row.JobID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt))
	}
	return actions, nil
}

func (s Store) Evidence(ctx context.Context, jobID string) ([]core.Evidence, error) {
	rows, err := dbsql.New(s.DB).ListEvidence(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var records []core.Evidence
	for _, row := range rows {
		records = append(records, core.Evidence{ID: row.ID, Digest: row.Digest, ByteSize: row.ByteSize, MediaType: row.MediaType, Producer: row.Producer, Kind: row.Kind, ActionID: row.ActionID, AgentRunID: row.AgentRunID, Revision: row.Revision, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt})
	}
	return records, nil
}

func (s Store) NextWakeSequence(ctx context.Context, jobID string) (int64, error) {
	return dbsql.New(s.DB).NextWakeSequence(ctx, jobID)
}

func expectOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}

func expectOneRows(rows int64, err error) error {
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotFound
	}
	return nil
}
