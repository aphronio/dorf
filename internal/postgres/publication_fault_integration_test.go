package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/evidence"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/proofbarrier"
	publicationapi "github.com/aphronio/dorf/internal/publication"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const publicationFaultChild = "DORF_TEST_PUBLICATION_FAULT_CHILD"

type publicationAuthorityState struct {
	mu          sync.Mutex
	base        string
	head        string
	branch      string
	title       string
	body        string
	pull        bool
	pushes      int
	pullCreates int
	pullUpdates int
}

func (s *publicationAuthorityState) handler(w http.ResponseWriter, request *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/git/ref/heads/greenfield"):
		_, _ = fmt.Fprintf(w, `{"object":{"sha":%q}}`, s.base)
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/git/ref/heads/"+s.branch):
		if s.head == "" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"object":{"sha":%q}}`, s.head)
	case request.Method == http.MethodPost && request.URL.Path == "/__test/push":
		var pushed struct {
			Revision string `json:"revision"`
			Branch   string `json:"branch"`
		}
		if err := json.NewDecoder(request.Body).Decode(&pushed); err != nil || pushed.Revision == "" || pushed.Branch != s.branch {
			http.Error(w, `{"message":"invalid exact push"}`, http.StatusBadRequest)
			return
		}
		s.pushes++
		s.head = pushed.Revision
		_, _ = w.Write([]byte(`{"accepted":true}`))
	case request.Method == http.MethodGet && request.URL.Path == "/repos/aphronio/dorf/pulls":
		if request.URL.Query().Get("state") != "all" || request.URL.Query().Get("head") != "aphronio:"+s.branch || request.URL.Query().Has("base") {
			http.Error(w, `{"message":"unsafe lookup"}`, http.StatusBadRequest)
			return
		}
		if !s.pull {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode([]any{s.pullPayload()})
	case request.Method == http.MethodPost && request.URL.Path == "/repos/aphronio/dorf/pulls":
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Draft bool   `json:"draft"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Head != s.branch || body.Base != "greenfield" || body.Draft || s.head == "" {
			http.Error(w, `{"message":"invalid pull request"}`, http.StatusBadRequest)
			return
		}
		s.pullCreates++
		s.pull, s.title, s.body = true, body.Title, body.Body
		_ = json.NewEncoder(w).Encode(s.pullPayload())
	case request.Method == http.MethodPatch && request.URL.Path == "/repos/aphronio/dorf/pulls/43":
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Base  string `json:"base"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || !s.pull || body.Base != "greenfield" {
			http.Error(w, `{"message":"invalid pull update"}`, http.StatusBadRequest)
			return
		}
		s.pullUpdates++
		s.title, s.body = body.Title, body.Body
		_ = json.NewEncoder(w).Encode(s.pullPayload())
	default:
		http.Error(w, `{"message":"unexpected test authority request"}`, http.StatusNotFound)
	}
}

func (s *publicationAuthorityState) pullPayload() map[string]any {
	return map[string]any{
		"number": 43, "html_url": "https://github.com/aphronio/dorf/pull/43",
		"title": s.title, "state": "open", "draft": false, "body": s.body,
		"head": map[string]any{"ref": s.branch, "sha": s.head, "repo": map[string]any{"full_name": "aphronio/dorf"}},
		"base": map[string]any{"ref": "greenfield"},
	}
}

type publicationFaultRepository struct{ URL string }

func (publicationFaultRepository) Relation(context.Context, spine.Job, string) (string, error) {
	return "", fmt.Errorf("unexpected ancestry classification in exact accepted-effect recovery")
}

func (r publicationFaultRepository) Push(ctx context.Context, job spine.Job, token string) error {
	if token == "" {
		return fmt.Errorf("missing ephemeral token")
	}
	contents, err := json.Marshal(map[string]string{"revision": job.Revision, "branch": job.Branch})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/__test/push", bytes.NewReader(contents))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("test repository push returned HTTP %d", response.StatusCode)
	}
	return nil
}

func publicationFaultGitHub(apiURL string) githubapi.Client {
	return githubapi.Client{APIURL: apiURL, Mint: func(context.Context, githubapi.Authority, string, string) (string, error) {
		return "ephemeral-test-token", nil
	}}
}

func publicationFaultInput(admissionKey string) postgres.NewJob {
	return postgres.NewJob{
		AdmissionKey:         admissionKey,
		Goal:                 "recover publication after process loss",
		Repository:           "https://github.com/aphronio/dorf.git",
		Revision:             strings.Repeat("a", 40),
		Branch:               "dorf/publication-fault-recovery",
		ProviderConnection:   "primary",
		ProviderGatewayState: "/tmp/dorf-provider-gateway-test",
		Model:                "gpt-5.6-sol",
		ReasoningEffort:      "high",
		GitHubRepository:     "aphronio/dorf",
		GitHubInstallation:   "42",
		BaseBranch:           "greenfield",
	}
}

func TestPostgresPublicationAcceptedEffectSIGKILLRecovery(t *testing.T) {
	if os.Getenv(publicationFaultChild) == "1" {
		runPublicationFaultChild(t)
		return
	}
	for _, point := range []string{spine.BarrierPushAccepted, spine.BarrierPullRequestAccepted} {
		t.Run(point, func(t *testing.T) {
			db, store, client := testDatabase(t)
			ctx := context.Background()
			input := publicationFaultInput(fmt.Sprintf("publication-sigkill-%s-%d", point, time.Now().UnixNano()))
			state := &publicationAuthorityState{base: strings.Repeat("b", 40), branch: input.Branch}
			server := httptest.NewServer(http.HandlerFunc(state.handler))
			defer server.Close()
			evidenceRoot := t.TempDir()
			job := preparePublicationFaultJob(t, db, store, input, evidenceRoot)
			service := publicationapi.Service{Store: store, GitHub: publicationFaultGitHub(server.URL), Repository: publicationFaultRepository{URL: server.URL}, Evidence: evidence.Store{Root: evidenceRoot}}
			publicationapi.Register(client, service)
			_, taskID, created, err := publicationapi.Schedule(ctx, store, client, nil, job.ID, job.Revision)
			if err != nil || !created || taskID == "" {
				t.Fatalf("schedule task=%s created=%v err=%v", taskID, created, err)
			}
			t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, taskID) })
			push, pull, err := store.PublicationActions(ctx, job.ID, job.Revision)
			if err != nil {
				t.Fatal(err)
			}
			identity := push.ID
			if point == spine.BarrierPullRequestAccepted {
				identity = pull.ID
			}
			barrierDir := t.TempDir()
			command := publicationFaultWorkerCommand(t, job.ID, point, barrierDir, server.URL, evidenceRoot)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(barrierDir, fmt.Sprintf("%s-%s-%s.ready", job.ID, identity, point))
			if err := waitForPublicationFaultMarker(marker, 6*time.Second); err != nil {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("barrier marker: %v\nchild output:\n%s", err, output.String())
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = command.Wait()
			var exit *exec.ExitError
			status := syscall.WaitStatus(0)
			if errors.As(err, &exit) {
				status, _ = exit.Sys().(syscall.WaitStatus)
			}
			if !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("worker was not lost with SIGKILL: %v output=%s", err, output.String())
			}
			if point == spine.BarrierPullRequestAccepted {
				proposal, proposalErr := store.Proposal(ctx, job.ID)
				_, retainedPull, actionErr := store.PublicationActions(ctx, job.ID, job.Revision)
				if proposalErr != nil || actionErr != nil || proposal != nil || retainedPull.State != spine.ActionUncertain {
					t.Fatalf("lost accepted PR proposal=%#v pull=%#v errors=%v/%v", proposal, retainedPull, proposalErr, actionErr)
				}
				if _, err := workflow.ScheduleCleanup(ctx, store, client, job.ID); err == nil || !strings.Contains(err.Error(), "publication") {
					t.Fatalf("uncertain pull cleanup error=%v", err)
				}
			}

			// The proof barrier shortens the live Absurd lease to ten seconds.
			time.Sleep(11 * time.Second)
			if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "publication-fault-rescue", ClaimTimeout: 30 * time.Second, BatchSize: 1}); err != nil {
				t.Fatal(err)
			}
			if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "publication-fault-recovery", ClaimTimeout: 30 * time.Second, BatchSize: 1}); err != nil {
				t.Fatal(err)
			}
			taskResult, err := client.FetchTaskResult(ctx, config.QueueName, taskID)
			if err != nil || taskResult == nil || taskResult.State != absurd.TaskCompleted {
				t.Fatalf("recovered public task result=%#v err=%v", taskResult, err)
			}
			job, err = store.Job(ctx, job.ID)
			proposal, proposalErr := store.Proposal(ctx, job.ID)
			retainedPush, retainedPull, actionsErr := store.PublicationActions(ctx, job.ID, job.Revision)
			if err != nil || proposalErr != nil || actionsErr != nil || job.WorkflowPhase != "published" || proposal == nil || proposal.Stale || proposal.ObservedRemoteHead != job.Revision || job.PublicationTaskID != taskID || retainedPush.State != spine.ActionSucceeded || retainedPull.State != spine.ActionSucceeded {
				t.Fatalf("converged Job=%#v proposal=%#v push=%#v pull=%#v errors=%v/%v/%v", job, proposal, retainedPush, retainedPull, err, proposalErr, actionsErr)
			}
			state.mu.Lock()
			remote, pushes, pulls, updates := state.head, state.pushes, state.pullCreates, state.pullUpdates
			state.mu.Unlock()
			if remote != job.Revision || pushes != 1 || pulls != 1 || updates != 0 {
				t.Fatalf("external authority remote=%s pushes=%d pull-creates=%d pull-updates=%d", remote, pushes, pulls, updates)
			}
			var proposals int
			if err := db.QueryRowContext(ctx, `select count(*) from dorf.github_proposals where job_id=$1`, job.ID).Scan(&proposals); err != nil {
				t.Fatal(err)
			}
			if proposals != 1 || strings.Contains(retainedPush.ExternalID+retainedPull.ExternalID+retainedPull.Outcome, "ephemeral-test-token") {
				t.Fatalf("durable receipts proposals=%d push=%#v pull=%#v", proposals, retainedPush, retainedPull)
			}
		})
	}
}

func preparePublicationFaultJob(t *testing.T, db *sql.DB, store postgres.Store, input postgres.NewJob, evidenceRoot string) spine.Job {
	t.Helper()
	ctx := context.Background()
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	command := "go test ./internal/publication"
	checkID := spine.CheckID(job.ID, job.Revision, "check")
	evidenceID := spine.EvidenceID(checkID, "check-output")
	now := time.Now().UTC().Truncate(time.Microsecond)
	artifact, err := json.Marshal(map[string]any{
		"identity": checkID, "revision": job.Revision, "producer": "dorf-go-worker", "provenance": "observed",
		"command": command, "exit_code": 0, "started_at": now, "finished_at": now,
		"stdout": "focused publication proof passed", "stderr": "", "stdout_truncated": false, "stderr_truncated": false, "redactions": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := (evidence.Store{Root: evidenceRoot}).Put(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.repository_commands(job_id,name,command) values($1,'check',$2)`, job.ID, command); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.checks(id,job_id,name,command,revision,state,exit_code,started_at,finished_at) values($1,$2,'check',$3,$4,'passed',0,$5,$5)`, checkID, job.ID, command, job.Revision, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.evidence(id,job_id,digest,byte_size,media_type,producer,provenance,kind,check_id,revision,started_at,finished_at) values($1,$2,$3,$4,'application/vnd.dorf.observation+json','dorf-go-worker','observed','check-output',$5,$6,$7,$7)`, evidenceID, job.ID, blob.Digest, blob.ByteSize, checkID, job.Revision, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.checks set evidence_id=$2 where id=$1`, checkID, evidenceID); err != nil {
		t.Fatal(err)
	}
	// Keep the fixture aligned with the current one-plan-per-Revision schema.
	// The former final_plan column was part of the retired review-plan shape;
	// publication only needs the finalized plan and its corresponding facts.
	facts := fmt.Sprintf(`{"revision":%q,"base_revision":%q,"paths":["internal/publication"],"checks_green":true,"documentation_only":false,"browser_ui":false,"authentication_authority":false,"declared_performance":false,"unknown":false}`, job.Revision, job.StartingRevision)
	plan := `{"decision":"no-review","roles":[],"reasons":[]}`
	if _, err := db.ExecContext(ctx, `insert into dorf.review_plans(job_id,revision,state,facts,plan,finalized_at) values($1,$2,'final',$3::jsonb,$4::jsonb,$5)`, job.ID, job.Revision, facts, plan, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func publicationFaultWorkerCommand(t *testing.T, jobID, point, barrierDir, apiURL, evidenceRoot string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestPostgresPublicationAcceptedEffectSIGKILLRecovery$", "-test.v")
	command.Env = append(os.Environ(),
		publicationFaultChild+"=1",
		"DORF_TEST_PUBLICATION_FAULT_API_URL="+apiURL,
		"DORF_TEST_PUBLICATION_FAULT_EVIDENCE_ROOT="+evidenceRoot,
		"DORF_PROOF_FAULT_BARRIER="+point,
		"DORF_PROOF_FAULT_BARRIER_JOB="+jobID,
		"DORF_PROOF_FAULT_BARRIER_DIR="+barrierDir,
		"DORF_PROOF_FAULT_BARRIER_ENABLE=issue-43-external-sigkill-only",
	)
	return command
}

func runPublicationFaultChild(t *testing.T) {
	db, err := sql.Open("pgx", os.Getenv("DORF_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := postgres.Store{DB: db}
	client, err := absurd.New(absurd.Options{DB: db, QueueName: config.QueueName})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	barrier, err := proofbarrier.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	apiURL := os.Getenv("DORF_TEST_PUBLICATION_FAULT_API_URL")
	service := publicationapi.Service{Store: store, GitHub: publicationFaultGitHub(apiURL), Repository: publicationFaultRepository{URL: apiURL}, Evidence: evidence.Store{Root: os.Getenv("DORF_TEST_PUBLICATION_FAULT_EVIDENCE_ROOT")}, Barrier: barrier}
	publicationapi.Register(client, service)
	if err := client.WorkBatch(context.Background(), absurd.WorkBatchOptions{WorkerID: "publication-fault-child", ClaimTimeout: 30 * time.Second, BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
}

func waitForPublicationFaultMarker(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}
