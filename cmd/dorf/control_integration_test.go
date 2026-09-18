package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestControlAPIMultipartAttachmentsPersistAndReplayAfterCleanup(t *testing.T) {
	ctx := context.Background()
	store, tasks, profileName := controlTestStore(t)
	provider := controlTestGateway(t)
	auth := controlauth.Service{Store: store}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.IssueKey(ctx, "attachment-client", credential); err != nil {
		t.Fatal(err)
	}
	runtimes := controlTestRuntimes{profile: profileName}
	blobRoot := t.TempDir()
	handler := controlTestHandler(store, tasks, provider, auth, runtimes, blob.Store{Root: blobRoot})
	jobKey := fmt.Sprintf("attachment-job-%d", time.Now().UnixNano())
	jobResponse := controlTestRequest(t, handler, http.MethodPost, "/v1/jobs", credential, jobKey, controlapi.AdmitJobRequest{
		AgentsMD: "attachment integration", AIConnection: "primary", Model: "model-test", Reasoning: "high",
	})
	var job controlapi.DirectJob
	controlTestJSON(t, jobResponse, http.StatusCreated, &job)

	imageBytes := encodeTestPNG(t, 3, 2)
	fileBytes := []byte("generic attachment bytes")
	attachments := []controlapi.SendMessageAttachment{
		{Filename: "diagram-ä.png", Contents: imageBytes},
		{Filename: "notes.txt", Contents: fileBytes},
	}
	messageKey := fmt.Sprintf("attachment-message-%d", time.Now().UnixNano())
	firstResponse := controlTestMultipartMessage(t, handler, job.ID, credential, messageKey, "", "follow", attachments)
	var first controlapi.Message
	controlTestJSON(t, firstResponse, http.StatusCreated, &first)
	execution, err := store.AgentMessageExecution(ctx, first.ID)
	if err != nil || execution.Message.Input != "" || len(execution.Message.Attachments) != 2 {
		t.Fatalf("durable attachment execution=%#v err=%v", execution, err)
	}
	imageDigest, fileDigest := sha256.Sum256(imageBytes), sha256.Sum256(fileBytes)
	wantDigests := []string{fmt.Sprintf("%x", imageDigest), fmt.Sprintf("%x", fileDigest)}
	wantKinds := []core.MessageAttachmentKind{core.MessageAttachmentImage, core.MessageAttachmentFile}
	for index, attachment := range execution.Message.Attachments {
		if attachment.Filename != attachments[index].Filename || attachment.Digest != wantDigests[index] ||
			attachment.ByteSize != int64(len(attachments[index].Contents)) || attachment.Kind != wantKinds[index] {
			t.Fatalf("durable attachment %d=%#v", index, attachment)
		}
		if err := (blob.Store{Root: blobRoot}).Verify(attachment.Digest, attachment.ByteSize); err != nil {
			t.Fatalf("verify attachment %d: %v", index, err)
		}
	}

	restarted := controlTestHandler(store, tasks, provider, controlauth.Service{Store: store}, runtimes, blob.Store{Root: blobRoot})
	replayResponse := controlTestMultipartMessage(t, restarted, job.ID, credential, messageKey, "", "follow", attachments)
	var replayed controlapi.Message
	controlTestJSON(t, replayResponse, http.StatusOK, &replayed)
	if replayed.ID != first.ID {
		t.Fatalf("replayed Message=%#v, want ID %s", replayed, first.ID)
	}
	changed := append([]controlapi.SendMessageAttachment(nil), attachments...)
	changed[0].Contents = encodeTestPNG(t, 4, 2)
	var conflict controlapi.Problem
	controlTestJSON(t, controlTestMultipartMessage(t, restarted, job.ID, credential, messageKey, "", "follow", changed), http.StatusConflict, &conflict)
	if conflict.Code != "idempotency_conflict" {
		t.Fatalf("changed attachment conflict=%#v", conflict)
	}

	deliveriesBeforeCleanup, err := store.Deliveries(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	postCleanupResponse := controlTestMultipartMessage(t, restarted, job.ID, credential, messageKey, "", "follow", attachments)
	var postCleanup controlapi.Message
	controlTestJSON(t, postCleanupResponse, http.StatusOK, &postCleanup)
	deliveriesAfterCleanup, err := store.Deliveries(ctx, job.ID)
	if err != nil || postCleanup.ID != first.ID || len(deliveriesAfterCleanup) != len(deliveriesBeforeCleanup) {
		t.Fatalf("post-cleanup replay=%#v deliveries=%d/%d err=%v", postCleanup, len(deliveriesAfterCleanup), len(deliveriesBeforeCleanup), err)
	}
}

func TestControlAPIObservationIntentDefaultsAndReplay(t *testing.T) {
	ctx := context.Background()
	store, tasks, profile := controlTestStore(t)
	auth := controlauth.Service{Store: store}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.IssueKey(ctx, "observation-default-client", credential); err != nil {
		t.Fatal(err)
	}
	handler := controlTestHandler(store, tasks, controlTestGateway(t), auth, controlTestRuntimes{profile: profile}, blob.Store{Root: t.TempDir()})
	key := fmt.Sprintf("observation-default-%d", time.Now().UnixNano())
	var job controlapi.DirectJob
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodPost, "/v1/jobs", credential, key, controlapi.AdmitJobRequest{
		AIConnection: "primary", Model: "model-test", Reasoning: "high",
	}), http.StatusCreated, &job)
	path := "/v1/jobs/" + job.ID + "/messages"
	for _, tc := range []struct {
		name   string
		intent string
		want   core.MessageDeliveryIntent
	}{
		{name: "omitted", want: core.MessageAuto},
		{name: "auto", intent: "auto", want: core.MessageAuto},
		{name: "follow", intent: "follow", want: core.MessageFollow},
		{name: "steer", intent: "steer"},
		{name: "unknown", intent: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := controlapi.SendMessageRequest{Text: "An external operation completed.", Observation: true, Intent: tc.intent}
			response := controlTestRequest(t, handler, http.MethodPost, path, credential, key+tc.name, input)
			if tc.want == "" {
				var problem controlapi.Problem
				controlTestJSON(t, response, http.StatusUnprocessableEntity, &problem)
				if problem.Code != "invalid_input" {
					t.Fatalf("unexpected rejection: %+v", problem)
				}
				return
			}
			var message controlapi.Message
			controlTestJSON(t, response, http.StatusCreated, &message)
			execution, err := store.AgentMessageExecution(ctx, message.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !execution.Message.Observation || execution.Message.RequestedIntent != tc.want || message.Intent != "follow" {
				t.Fatalf("observation=%t requested=%s effective=%s", execution.Message.Observation, execution.Message.RequestedIntent, message.Intent)
			}
			input.Intent = string(tc.want)
			var replay controlapi.Message
			controlTestJSON(t, controlTestRequest(t, handler, http.MethodPost, path, credential, key+tc.name, input), http.StatusOK, &replay)
			if replay.ID != message.ID {
				t.Fatalf("replay created a different Message: %s != %s", replay.ID, message.ID)
			}
		})
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 3 {
		t.Fatalf("deliveries=%d want=3 err=%v", len(deliveries), err)
	}
}

func TestControlAPIPostgresReplayRestartAndCleanup(t *testing.T) {
	ctx := context.Background()
	store, firstTasks, profileName := controlTestStore(t)
	provider := controlTestGateway(t)
	auth := controlauth.Service{Store: store}
	enrollment, err := auth.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	fileBytes := []byte{0, 1, '\n', 255}
	runtimes := controlTestRuntimes{profile: profileName, contents: fileBytes}
	first := controlTestHandler(store, firstTasks, provider, auth, runtimes, blob.Store{Root: t.TempDir()})
	redeem := controlTestRequest(t, first, http.MethodPost, "/v1/auth/enrollments/redeem", "", "", controlapi.RedeemRequest{
		EnrollmentCode: enrollment.Token, ClientName: profileName, Credential: credential,
	})
	if redeem.Code != http.StatusCreated {
		t.Fatalf("redeem status=%d body=%s", redeem.Code, redeem.Body.String())
	}

	key := fmt.Sprintf("control-api-replay-%d", time.Now().UnixNano())
	input := controlapi.AdmitJobRequest{ClientReference: "agent0:conversation:example", AgentsMD: "prove remote durable replay", AIConnection: "primary", Model: "model-test", Reasoning: "high"}
	// The response is deliberately discarded after the handler commits, matching
	// a client that cannot know whether its first request succeeded.
	lost := controlTestRequest(t, first, http.MethodPost, "/v1/jobs", credential, key, input)
	if lost.Code != http.StatusCreated {
		t.Fatalf("first admission status=%d body=%s", lost.Code, lost.Body.String())
	}
	committed, err := store.Job(ctx, core.JobID(key))
	if err != nil || committed.CurrentTaskID == "" || committed.ProviderConnection != input.AIConnection {
		t.Fatalf("committed Job=%#v err=%v", committed, err)
	}
	// Replay must use the retained profile and AI connection even when the
	// deployment defaults can no longer be consulted.
	if err := os.Remove(filepath.Join(provider.StatePath, "connections.json")); err != nil {
		t.Fatal(err)
	}

	restartedTasks := controlTestTasks(t, store.DB, firstTasks.QueueName(), false)
	restarted := controlTestHandler(store, restartedTasks, provider, controlauth.Service{Store: store}, runtimes, blob.Store{Root: t.TempDir()})
	replay := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key, input)
	var replayed controlapi.DirectJob
	controlTestJSON(t, replay, http.StatusOK, &replayed)
	afterReplay, err := store.Job(ctx, committed.ID)
	if err != nil || replayed.ID != committed.ID || afterReplay.CurrentTaskID != committed.CurrentTaskID {
		t.Fatalf("replay Job=%#v durable=%#v err=%v", replayed, afterReplay, err)
	}

	creator, err := auth.Authenticate(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.CreatedByClient == nil || replayed.CreatedByClient.ID != creator.ID || replayed.CreatedByClient.Name != profileName || replayed.ClientReference != input.ClientReference {
		t.Fatalf("replayed attribution=%+v reference=%q", replayed.CreatedByClient, replayed.ClientReference)
	}
	secondCredential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.IssueKey(ctx, "other-client", secondCredential); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Revoke(ctx, creator.ID); err != nil {
		t.Fatal(err)
	}
	credential = secondCredential
	crossClientReplay := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key, input)
	controlTestJSON(t, crossClientReplay, http.StatusOK, &replayed)
	if replayed.CreatedByClient == nil || replayed.CreatedByClient.ID != creator.ID || replayed.CreatedByClient.Name != profileName {
		t.Fatalf("replay reassigned revoked creator: %+v", replayed.CreatedByClient)
	}
	var listed controlapi.JobList
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs?limit=100", credential, "", nil), http.StatusOK, &listed)
	found := false
	for _, item := range listed.Jobs {
		if item.ID == committed.ID {
			found = true
			if item.CreatedByClient == nil || *item.CreatedByClient != *replayed.CreatedByClient || item.ClientReference != input.ClientReference {
				t.Fatalf("listed attribution=%+v", item)
			}
		}
	}
	if !found {
		t.Fatal("attributed Job missing from index")
	}
	changedReference := input
	changedReference.ClientReference = "another-thread"
	var referenceConflict controlapi.Problem
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key, changedReference), http.StatusConflict, &referenceConflict)
	if referenceConflict.Code != "idempotency_conflict" {
		t.Fatalf("reference conflict=%+v", referenceConflict)
	}
	spoof := map[string]any{"created_by_client": map[string]string{"id": creator.ID, "name": profileName}}
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key+"-spoof", spoof), http.StatusBadRequest, &referenceConflict)

	var problem controlapi.Problem

	messageKey := fmt.Sprintf("control-message-%d", time.Now().UnixNano())
	messageInput := controlapi.SendMessageRequest{Text: "continue before the initial Turn settles", RefreshSkills: true}
	early := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey, messageInput)
	var accepted controlapi.Message
	controlTestJSON(t, early, http.StatusCreated, &accepted)
	if accepted.JobID != committed.ID || accepted.Sequence != 1 || accepted.Delivery.State != "accepted" {
		t.Fatalf("early Message=%#v", accepted)
	}
	execution, err := store.AgentMessageExecution(ctx, accepted.ID)
	if err != nil || !execution.Message.RefreshSkills {
		t.Fatalf("durable refresh=%+v err=%v", execution.Message, err)
	}
	restarted = controlTestHandler(store, restartedTasks, provider, controlauth.Service{Store: store}, runtimes, blob.Store{Root: t.TempDir()})
	changedRefresh := messageInput
	changedRefresh.RefreshSkills = false
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey, changedRefresh), http.StatusConflict, &problem)
	if problem.Code != "idempotency_conflict" {
		t.Fatalf("changed refresh=%+v", problem)
	}
	replayedMessage := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey, messageInput)
	var sameMessage controlapi.Message
	controlTestJSON(t, replayedMessage, http.StatusOK, &sameMessage)
	if sameMessage.ID != accepted.ID {
		t.Fatalf("replayed Message=%#v want ID %s", sameMessage, accepted.ID)
	}
	changedMessage := messageInput
	changedMessage.Text = "different input"
	messageConflict := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey, changedMessage)
	controlTestJSON(t, messageConflict, http.StatusConflict, &problem)
	if problem.Code != "idempotency_conflict" {
		t.Fatalf("Message conflict=%#v", problem)
	}

	owned, err := store.Sandbox(ctx, core.MainSandboxName(committed.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithJobFence(ctx, committed.ID, func() error {
		return store.BindSandboxResource(ctx, owned, "synthetic-provider-original")
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []core.ActionKind{core.ActionSandboxCreate, core.ActionRouteCreate} {
		action, err := store.GetOrCreateSandboxAction(ctx, core.MainSandboxName(committed.ID), kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	assertExecution := func(want string) {
		t.Helper()
		var inspected controlapi.DirectJob
		response := controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID, credential, "", nil)
		if strings.Contains(response.Body.String(), owned.OwnershipNonce) || strings.Contains(response.Body.String(), "ownership_nonce") {
			t.Fatal("Job inspection exposed ownership material")
		}
		controlTestJSON(t, response, http.StatusOK, &inspected)
		if inspected.Execution.State != want || inspected.Attention != nil {
			t.Fatalf("execution=%+v attention=%+v, want %s without attention", inspected.Execution, inspected.Attention, want)
		}
		if len(inspected.Sandboxes) != 1 || len(inspected.Sandboxes[0].Resources) != 1 {
			t.Fatal("inspection omitted retained provider resources")
		}
		resource := inspected.Sandboxes[0].Resources[0]
		if resource.ID != owned.ResourceID || resource.ProviderID != "synthetic-provider-original" || resource.ObservedAt == nil || resource.DeletedAt != nil {
			t.Fatalf("inspection resource=%+v", resource)
		}
	}
	assertExecution("awaiting_agent")

	initialRun := core.AgentRunID(accepted.ID)
	if err := store.PrepareAgentRun(ctx, initialRun, "codex", ""); err != nil {
		t.Fatal(err)
	}
	assertExecution("awaiting_agent")
	if err := store.BindAgentRun(ctx, initialRun, "codex", "control-thread", "control-turn", "inProgress"); err != nil {
		t.Fatal(err)
	}
	assertExecution("running")
	auto := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey+"-auto", controlapi.SendMessageRequest{Text: "correct the active answer", RefreshSkills: true})
	var steering controlapi.Message
	controlTestJSON(t, auto, http.StatusCreated, &steering)
	ordinary, err := store.AgentMessageExecution(ctx, steering.ID)
	if err != nil || !ordinary.Message.RefreshSkills || ordinary.RefreshSkills {
		t.Fatalf("ordinary refresh=%+v err=%v", ordinary.Message, err)
	}
	if steering.Intent != "steer" {
		t.Fatalf("omitted intent did not steer active work: %+v", steering)
	}
	if err := store.PrepareAgentRun(ctx, core.AgentRunID(steering.ID), "codex", "control-turn"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSteer(ctx, core.AgentRunID(steering.ID), "control-turn", "inProgress"); err != nil {
		t.Fatal(err)
	}
	readSteer := controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID+"/messages/"+steering.ID, credential, "", nil)
	controlTestJSON(t, readSteer, http.StatusOK, &steering)
	if steering.Result != nil {
		t.Fatalf("delivered steer fabricated a terminal reply: %+v", steering)
	}
	assertExecution("running")
	interruptPath := "/v1/jobs/" + committed.ID + "/messages/" + steering.ID + "/interrupt"
	for range 2 {
		stop := controlTestRequest(t, restarted, http.MethodPut, interruptPath, credential, "", nil)
		var stopped controlapi.Message
		controlTestJSON(t, stop, http.StatusOK, &stopped)
		if !stopped.InterruptRequested || stopped.ID != steering.ID || stopped.Result != nil {
			t.Fatalf("interrupt did not return exact accepted custody: %+v", stopped)
		}
	}
	missingStop := controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+committed.ID+"/messages/missing/interrupt", credential, "", nil)
	controlTestJSON(t, missingStop, http.StatusNotFound, &problem)
	if problem.Code != "message_not_found" {
		t.Fatalf("unknown interrupt target=%+v", problem)
	}

	file := controlTestRequest(t, restarted, http.MethodGet, "/v1/sandboxes/"+replayed.Sandboxes[0].ID+"/files?path=result.bin", credential, "", nil)
	if file.Code != http.StatusOK || !bytes.Equal(file.Body.Bytes(), fileBytes) {
		t.Fatalf("Sandbox file status=%d bytes=%v", file.Code, file.Body.Bytes())
	}
	invalidFile := controlTestRequest(t, restarted, http.MethodGet, "/v1/sandboxes/"+replayed.Sandboxes[0].ID+"/files?path=../secret", credential, "", nil)
	controlTestJSON(t, invalidFile, http.StatusUnprocessableEntity, &problem)
	if problem.Code != "invalid_file_path" {
		t.Fatalf("invalid file problem=%#v", problem)
	}
	missingFile := controlTestRequest(t, restarted, http.MethodGet, "/v1/sandboxes/missing-sandbox/files?path=result.bin", credential, "", nil)
	controlTestJSON(t, missingFile, http.StatusNotFound, &problem)
	if problem.Code != "sandbox_not_found" {
		t.Fatalf("missing Sandbox problem=%#v", problem)
	}
	evidence := controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID+"/evidence", credential, "", nil)
	var retained controlapi.EvidenceList
	controlTestJSON(t, evidence, http.StatusOK, &retained)
	if retained.Evidence == nil || len(retained.Evidence) != 0 {
		t.Fatalf("direct Job Evidence=%#v, want an explicit empty collection", retained)
	}

	retryKey := fmt.Sprintf("control-retry-%d", time.Now().UnixNano())
	notEligible := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/retries", credential, retryKey, nil)
	controlTestJSON(t, notEligible, http.StatusConflict, &problem)
	if problem.Code != "retry_unavailable" {
		t.Fatalf("retry problem=%#v", problem)
	}

	changed := input
	changed.AgentsMD = "different input must conflict"
	conflict := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key, changed)
	controlTestJSON(t, conflict, http.StatusConflict, &problem)
	if problem.Code != "idempotency_conflict" {
		t.Fatalf("conflict=%#v", problem)
	}

	if err := store.InterruptAgentRun(ctx, initialRun, "test turn finished"); err != nil {
		t.Fatal(err)
	}
	var latest controlapi.Message
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID+"/messages/latest", credential, "", nil), http.StatusOK, &latest)
	if latest.ID != accepted.ID || latest.Result == nil || latest.Result.Outcome != "interrupted" {
		t.Fatalf("latest reply=%+v", latest)
	}
	var withReply controlapi.DirectJob
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID, credential, "", nil), http.StatusOK, &withReply)
	if withReply.LatestReplyID != latest.ID {
		t.Fatalf("inspection lost latest reply: %+v", withReply)
	}
	var pending controlapi.Message
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey+"-next", controlapi.SendMessageRequest{Text: "next task", Intent: "follow"}), http.StatusCreated, &pending)
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID+"/messages/latest", credential, "", nil), http.StatusOK, &latest)
	if latest.ID != accepted.ID {
		t.Fatal("pending work displaced the latest reply")
	}
	hold, err := store.HoldSandboxDelivery(ctx, restartedTasks.QueueName(), committed.ID, owned.ID, committed.ID+":upgrade")
	if err != nil {
		t.Fatal(err)
	}
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID+"/messages/"+pending.ID, credential, "", nil), http.StatusOK, &pending)
	if pending.WaitReason != "workspace_upgrade" || pending.Delivery.State != "accepted" || pending.Result != nil {
		t.Fatal("held Message was not reported as accepted and waiting without a result")
	}
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID, credential, "", nil), http.StatusOK, &withReply)
	if len(withReply.Sandboxes) != 1 || withReply.Sandboxes[0].DeliveryHold == nil || withReply.Sandboxes[0].DeliveryHold.ID != hold.ID {
		t.Fatal("Job inspection omitted the durable delivery hold")
	}
	if err := store.ReleaseSandboxDelivery(ctx, restartedTasks.QueueName(), committed.ID, owned.ID, hold.ID); err != nil {
		t.Fatal(err)
	}
	// Decode into a fresh DTO because omitted optional fields must not reuse a previous value.
	var released controlapi.Message
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID+"/messages/"+pending.ID, credential, "", nil), http.StatusOK, &released)
	if released.WaitReason != "" {
		t.Fatal("released Message retained a queue wait reason")
	}
	recoveryHoldID := committed.ID + ":recovery"
	if _, err := store.DB.ExecContext(ctx, `insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason) values($1,$2,'checkpoint_recovery')`, recoveryHoldID, owned.ID); err != nil {
		t.Fatal(err)
	}
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID+"/messages/"+pending.ID, credential, "", nil), http.StatusOK, &pending)
	if pending.WaitReason != "checkpoint_recovery" {
		t.Fatalf("recovery-held Message wait reason=%q", pending.WaitReason)
	}
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID, credential, "", nil), http.StatusOK, &withReply)
	if len(withReply.Sandboxes) != 1 || withReply.Sandboxes[0].DeliveryHold == nil || withReply.Sandboxes[0].DeliveryHold.Reason != "checkpoint_recovery" {
		t.Fatal("Job inspection omitted the checkpoint recovery hold reason")
	}
	if err := store.ReleaseSandboxDelivery(ctx, restartedTasks.QueueName(), committed.ID, owned.ID, recoveryHoldID); err != nil {
		t.Fatal(err)
	}

	cleanup := controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+committed.ID+"/cleanup", credential, "", nil)
	var cleaning controlapi.DirectJob
	controlTestJSON(t, cleanup, http.StatusOK, &cleaning)
	cleaningFact, err := store.Job(ctx, committed.ID)
	if err != nil || cleaning.Admission.Open || cleaning.Cleanup.State != "running" || cleaningFact.CurrentTaskID == committed.CurrentTaskID {
		t.Fatalf("cleanup view=%#v durable=%#v err=%v", cleaning, cleaningFact, err)
	}
	fencedFile := controlTestRequest(t, restarted, http.MethodGet, "/v1/sandboxes/"+replayed.Sandboxes[0].ID+"/files?path=result.bin", credential, "", nil)
	controlTestJSON(t, fencedFile, http.StatusConflict, &problem)
	if problem.Code != "file_unavailable" {
		t.Fatalf("cleanup-fenced file problem=%#v", problem)
	}

	// Recreate the API and Absurd client again: cleanup remains one durable
	// request and one attached task rather than being rescheduled.
	finalTasks := controlTestTasks(t, store.DB, firstTasks.QueueName(), false)
	finalHandler := controlTestHandler(store, finalTasks, provider, controlauth.Service{Store: store}, runtimes, blob.Store{Root: t.TempDir()})
	repeated := controlTestRequest(t, finalHandler, http.MethodPut, "/v1/jobs/"+committed.ID+"/cleanup", credential, "", nil)
	controlTestJSON(t, repeated, http.StatusOK, &cleaning)
	finalFact, err := store.Job(ctx, committed.ID)
	if err != nil || finalFact.CurrentTaskID != cleaningFact.CurrentTaskID || finalFact.CleanupState != core.CleanupScheduled {
		t.Fatalf("replayed cleanup durable=%#v err=%v", finalFact, err)
	}
	for _, kind := range []core.ActionKind{core.ActionRouteRevoke, core.ActionSandboxDelete} {
		action, err := store.GetOrCreateSandboxAction(ctx, owned.ID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	controlTestJSON(t, controlTestRequest(t, finalHandler, http.MethodGet, "/v1/jobs/"+committed.ID, credential, "", nil), http.StatusOK, &cleaning)
	if len(cleaning.Sandboxes) != 1 || len(cleaning.Sandboxes[0].Resources) != 1 || cleaning.Sandboxes[0].Resources[0].DeletedAt == nil {
		t.Fatal("Job inspection lost the resource deletion receipt after API restart")
	}
}

func TestControlAPIWorkflowAdmissionsProjectAndReplay(t *testing.T) {
	ctx := context.Background()
	store, tasks, profileName := controlTestStore(t)
	provider := controlTestGateway(t)
	auth := controlauth.Service{Store: store}
	enrollment, err := auth.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	installations := &controlTestGitHub{installation: "42"}
	handler := controlTestHandlerWithGitHub(store, tasks, provider, auth,
		controlTestRuntimes{profile: profileName}, blob.Store{Root: t.TempDir()}, installations)
	redeem := controlTestRequest(t, handler, http.MethodPost, "/v1/auth/enrollments/redeem", "", "", controlapi.RedeemRequest{
		EnrollmentCode: enrollment.Token, ClientName: profileName, Credential: credential,
	})
	if redeem.Code != http.StatusCreated {
		t.Fatalf("redeem status=%d body=%s", redeem.Code, redeem.Body.String())
	}

	codingKey := fmt.Sprintf("control-coding-%d", time.Now().UnixNano())
	codingInput := controlapi.AdmitCodingJobRequest{KeepRunning: true, ClientReference: "coding-task",
		Repository: "https://github.com/aphronio/dorf.git",
		Revision:   strings.Repeat("a", 40), BaseBranch: "main", Profile: profileName, AIConnection: "primary", Model: "model-test",
	}
	codingResponse := controlTestRequest(t, handler, http.MethodPost, "/v1/workflows/coding/jobs", credential, codingKey, codingInput)
	var codingJob controlapi.CodingJob
	controlTestJSON(t, codingResponse, http.StatusCreated, &codingJob)
	if !codingJob.KeepRunning || codingJob.CreatedByClient == nil || codingJob.CreatedByClient.Name != profileName || codingJob.ClientReference != "coding-task" || codingJob.Kind != controlapi.JobKindCoding ||
		codingJob.Branch != "dorf/"+core.JobID(codingKey) || codingJob.StartingRevision != codingInput.Revision ||
		codingJob.Revision != codingInput.Revision || codingJob.WorkflowRevision == "" || codingJob.Outcome != nil {
		t.Fatalf("coding Job=%#v", codingJob)
	}
	if installations.calls != 1 {
		t.Fatalf("coding installation discoveries=%d, want 1", installations.calls)
	}
	codingFact, err := store.Job(ctx, codingJob.ID)
	if err != nil || codingFact.CurrentTaskID == "" || codingFact.ProviderConnection != codingInput.AIConnection {
		t.Fatalf("coding durable Job=%#v err=%v", codingFact, err)
	}

	// A restarted API must replay the retained GitHub installation without
	// consulting current external discovery.
	restartedTasks := controlTestTasks(t, store.DB, tasks.QueueName(), false)
	unavailableGitHub := &controlTestGitHub{err: fmt.Errorf("GitHub must not be consulted during replay")}
	restarted := controlTestHandlerWithGitHub(store, restartedTasks, provider, controlauth.Service{Store: store},
		controlTestRuntimes{profile: profileName}, blob.Store{Root: t.TempDir()}, unavailableGitHub)
	replayCoding := controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/coding/jobs", credential, codingKey, codingInput)
	var sameCoding controlapi.CodingJob
	controlTestJSON(t, replayCoding, http.StatusOK, &sameCoding)
	replayedCodingFact, replayedCodingErr := store.Job(ctx, codingJob.ID)
	if sameCoding.ID != codingJob.ID || unavailableGitHub.calls != 0 || replayedCodingErr != nil || replayedCodingFact.CurrentTaskID != codingFact.CurrentTaskID {
		t.Fatalf("coding replay=%#v GitHub calls=%d durable=%#v err=%v", sameCoding, unavailableGitHub.calls, replayedCodingFact, replayedCodingErr)
	}
	changedCoding := codingInput
	changedCoding.BaseBranch = "develop"
	var codingConflict controlapi.Problem
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/coding/jobs", credential, codingKey, changedCoding), http.StatusConflict, &codingConflict)
	if codingConflict.Code != "idempotency_conflict" {
		t.Fatalf("coding replay conflict=%#v", codingConflict)
	}

	directKey := fmt.Sprintf("control-direct-%d", time.Now().UnixNano())
	directInput := controlapi.AdmitJobRequest{Profile: profileName, AIConnection: "primary", Model: "model-test"}
	var directJob controlapi.DirectJob
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, directKey, directInput), http.StatusCreated, &directJob)
	foreignKind := codingInput
	foreignKind.Profile = "missing-profile-must-not-be-resolved"
	foreignKindConflict := controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/coding/jobs", credential, directKey, foreignKind)
	var foreignKindProblem controlapi.Problem
	controlTestJSON(t, foreignKindConflict, http.StatusConflict, &foreignKindProblem)
	if foreignKindProblem.Code != "idempotency_conflict" || unavailableGitHub.calls != 0 {
		t.Fatalf("foreign-kind replay conflict=%#v GitHub calls=%d", foreignKindProblem, unavailableGitHub.calls)
	}
	message := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+codingJob.ID+"/messages", credential,
		"message-"+codingJob.ID, controlapi.SendMessageRequest{Text: "continue", Intent: "follow"})
	var accepted controlapi.Message
	controlTestJSON(t, message, http.StatusCreated, &accepted)
	if accepted.JobID != codingJob.ID || accepted.Sequence != 1 {
		t.Fatalf("workflow Message=%#v", accepted)
	}

	completedCodingResponse := controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+codingJob.ID+"/abandon", credential, "", nil)
	var completedCoding controlapi.CodingJob
	controlTestJSON(t, completedCodingResponse, http.StatusOK, &completedCoding)
	if completedCoding.Execution.State != "complete" || completedCoding.Cleanup.State != "running" || completedCoding.Outcome == nil || completedCoding.Outcome.Kind != string(coding.OutcomeAbandoned) {
		t.Fatalf("completed coding Job=%#v", completedCoding)
	}
	var replayedAbandon controlapi.CodingJob
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+codingJob.ID+"/abandon", credential, "", nil), http.StatusOK, &replayedAbandon)
	if replayedAbandon.Outcome == nil || replayedAbandon.Outcome.Kind != string(coding.OutcomeAbandoned) {
		t.Fatalf("replayed abandon=%#v", replayedAbandon)
	}
	cleanupDirect := controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+directJob.ID+"/cleanup", credential, "", nil)
	var cleaningDirect controlapi.DirectJob
	controlTestJSON(t, cleanupDirect, http.StatusOK, &cleaningDirect)
	if cleaningDirect.Cleanup.State != "running" || cleaningDirect.Execution.State != "stopped" {
		t.Fatalf("cleanup-fenced direct Job=%#v", cleaningDirect)
	}
}

func TestControlAPIJobListKeepsKeysetContinuity(t *testing.T) {
	ctx := context.Background()
	store, _, profileName := controlTestStore(t)
	auth := controlauth.Service{Store: store}
	enrollment, err := auth.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := auth.Redeem(ctx, enrollment.Token, "pagination-client", credential); err != nil || !created {
		t.Fatalf("redeem pagination Client: created=%t err=%v", created, err)
	}

	base := fmt.Sprintf("job-page-%d", time.Now().UnixNano())
	tiedAt := time.Now().UTC().AddDate(100, 0, 0).Truncate(time.Microsecond)
	type listedFixture struct {
		id       string
		workflow string
		revision string
		at       time.Time
	}
	fixtures := []listedFixture{
		{base + "-z", "", "", tiedAt},
		{base + "-y", string(coding.Workflow), coding.WorkflowRevision, tiedAt},
		{base + "-x", "", "", tiedAt.Add(-time.Second)},
		{base + "-w", "", "", tiedAt.Add(-2 * time.Second)},
		// A retained but unrecognized workflow revision must not consume a page slot.
		{base + "-unsupported", string(coding.Workflow), "unrecognized", tiedAt.Add(time.Second)},
	}
	insert := func(fixture listedFixture) {
		t.Helper()
		_, err := store.DB.ExecContext(ctx, `
insert into dorf.jobs(
    id,admission_key,workflow_name,workflow_revision,
    sandbox_profile,sandbox_profile_revision,provider_connection,model,reasoning_effort,admitted_at
) values($1,$2,$3,$4,$5,(select candidate_revision from dorf.sandbox_profiles where name=$5),'primary','model-test','high',$6)
`, fixture.id, "admission-"+fixture.id, fixture.workflow, fixture.revision, profileName, fixture.at)
		if err != nil {
			t.Fatalf("insert Job list fixture %s: %v", fixture.id, err)
		}
	}
	for _, fixture := range fixtures {
		insert(fixture)
	}
	t.Cleanup(func() {
		for _, fixture := range fixtures {
			if _, err := store.DB.ExecContext(context.Background(), `delete from dorf.jobs where id=$1`, fixture.id); err != nil {
				t.Errorf("delete Job list fixture %s: %v", fixture.id, err)
			}
		}
	})

	handler := controlapi.NewServer(controlapi.Discovery{Product: "dorf"}, auth, controlAPIJobs{store: store}, controlAPIProfiles{store: store}).Handler
	unsupported := controlTestRequest(t, handler, http.MethodGet, "/v1/jobs/"+fixtures[4].id, credential, "", nil)
	var unsupportedProblem controlapi.Problem
	controlTestJSON(t, unsupported, http.StatusNotFound, &unsupportedProblem)
	if unsupportedProblem.Code != "job_not_found" {
		t.Fatalf("unsupported-revision Job Problem=%#v", unsupportedProblem)
	}
	firstResponse := controlTestRequest(t, handler, http.MethodGet, "/v1/jobs?limit=2", credential, "", nil)
	var first controlapi.JobList
	controlTestJSON(t, firstResponse, http.StatusOK, &first)
	if len(first.Jobs) != 2 || first.Jobs[0].ID != fixtures[0].id || first.Jobs[0].Kind != controlapi.JobKindDirect ||
		first.Jobs[1].ID != fixtures[1].id || first.Jobs[1].Kind != controlapi.JobKindCoding || first.NextCursor == nil {
		t.Fatalf("first Job page=%#v", first)
	}

	newer := listedFixture{base + "-new", "", "", tiedAt.Add(3 * time.Second)}
	fixtures = append(fixtures, newer)
	insert(newer)
	secondResponse := controlTestRequest(t, handler, http.MethodGet,
		"/v1/jobs?limit=2&cursor="+url.QueryEscape(*first.NextCursor), credential, "", nil)
	var second controlapi.JobList
	controlTestJSON(t, secondResponse, http.StatusOK, &second)
	if len(second.Jobs) != 2 || second.Jobs[0].ID != fixtures[2].id || second.Jobs[0].Kind != controlapi.JobKindDirect ||
		second.Jobs[1].ID != fixtures[3].id || second.Jobs[1].Kind != controlapi.JobKindDirect {
		t.Fatalf("second Job page=%#v", second)
	}

	payload, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	payload = bytes.Replace(payload, []byte(`"v":1`), []byte(`"v":2`), 1)
	tamperedCursor := base64.RawURLEncoding.EncodeToString(payload)
	if tamperedCursor == *first.NextCursor {
		t.Fatal("cursor version tamper did not change the token")
	}
	tampered := controlTestRequest(t, handler, http.MethodGet,
		"/v1/jobs?cursor="+url.QueryEscape(tamperedCursor), credential, "", nil)
	var problem controlapi.Problem
	controlTestJSON(t, tampered, http.StatusBadRequest, &problem)
	if problem.Code != "invalid_cursor" {
		t.Fatalf("tampered cursor Problem=%#v", problem)
	}
}

func controlTestStore(t *testing.T) (postgres.Store, *absurd.Client, string) {
	t.Helper()
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.Store{DB: db}
	if err := store.Migrate(context.Background()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	profileName := fmt.Sprintf("control-api-%d", suffix)
	incusAuthorityHash, err := (deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}).AuthorityHash()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	profile, _, err := store.CreateSandboxProfile(context.Background(), core.SandboxProfile{
		Name: profileName, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("a", 64),
		IncusEndpointAuthorityHash: incusAuthorityHash, IncusProject: "dorf", IncusStoragePool: "default",
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB", IncusGatewayURL: "http://10.44.0.1:8317/v1",
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(context.Background(), profile.Name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(context.Background(), verification, "codex-test")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(context.Background(), verification)
	}
	if err == nil {
		_, err = store.SetDefaultSandboxProfile(context.Background(), profile.Name)
	}
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	queue := fmt.Sprintf("%s_control_%d", config.QueueName, suffix)
	tasks := controlTestTasks(t, db, queue, true)
	t.Cleanup(func() {
		if err := tasks.DropQueue(context.Background(), queue); err != nil {
			t.Errorf("drop test queue %q: %v", queue, err)
		}
		db.Close()
	})
	return store, tasks, profileName
}

func controlTestTasks(t *testing.T, db *sql.DB, queue string, create bool) *absurd.Client {
	t.Helper()
	tasks, err := absurd.New(absurd.Options{DB: db, QueueName: queue})
	if err == nil && create {
		err = tasks.CreateQueue(context.Background(), queue)
	}
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}

func controlTestGateway(t *testing.T) gateway.Gateway {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "model-test"}}})
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"credentials/openai-0123456789abcdef.key": "unused-test-secret\n",
		"connections.json":                        `[{"name":"primary","provider":"openai","auth_mode":"api_key","credential_ref":"openai-0123456789abcdef.key","default":true}]`,
		"authority.json":                          `{"guard_key":"guard-test","management_key":"management-test"}`,
		"broker.yaml":                             fmt.Sprintf("host: %q\nport: %s\n", parsed.Hostname(), parsed.Port()),
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(state, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return gateway.Gateway{StatePath: state, Client: server.Client()}
}

func controlTestHandler(store postgres.Store, tasks *absurd.Client, provider gateway.Gateway, auth controlauth.Service, runtimes core.SandboxRuntimeResolver, evidence blob.Store) http.Handler {
	return controlTestHandlerWithGitHub(store, tasks, provider, auth, runtimes, evidence,
		&controlTestGitHub{installation: "42"})
}

func controlTestHandlerWithGitHub(store postgres.Store, tasks *absurd.Client, provider gateway.Gateway, auth controlauth.Service, runtimes core.SandboxRuntimeResolver, evidence blob.Store, github coding.InstallationDiscovery) http.Handler {
	queueName := config.QueueName
	if tasks != nil {
		queueName = tasks.QueueName()
	}
	reader := controlreader.Service{Store: store, Runtimes: runtimes, Provider: provider, Installations: github}
	messageImages, _ := runtimes.(messageImageCapability)
	return controlapi.NewServer(controlapi.Discovery{Product: "dorf"}, auth,
		controlAPIJobs{
			store: store, tasks: tasks,
			directAdmissions: direct.NewAdmissionService(store, queueName, reader),
			codingAdmissions: coding.NewAdmissionService(store, queueName, reader, reader),
			reader:           reader, blobs: evidence, messageImages: messageImages,
		}, controlAPIProfiles{store: store}).Handler
}

type controlTestGitHub struct {
	installation string
	err          error
	calls        int
}

func (g *controlTestGitHub) DiscoverInstallation(context.Context, string) (string, error) {
	g.calls++
	return g.installation, g.err
}

type controlTestRuntimes struct {
	profile  string
	contents []byte
}

func (r controlTestRuntimes) ResolveSandbox(_ context.Context, profile core.SandboxProfileRef) (core.SandboxRuntime, error) {
	if profile.Name != r.profile {
		return core.SandboxRuntime{}, fmt.Errorf("unexpected Sandbox profile %q", profile)
	}
	return core.SandboxRuntime{SandboxProfile: profile, Files: r}, nil
}

func (r controlTestRuntimes) SupportsMessageImages(_ context.Context, profile core.SandboxProfileRef) (bool, error) {
	return profile.Name == r.profile, nil
}

func (r controlTestRuntimes) ReadSandboxFile(context.Context, core.Job, core.Sandbox, string) ([]byte, error) {
	return append([]byte(nil), r.contents...), nil
}

func controlTestRequest(t *testing.T, handler http.Handler, method, path, credential, key string, input any) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &body)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func controlTestMultipartMessage(t *testing.T, handler http.Handler, jobID, credential, key, text, intent string, attachments []controlapi.SendMessageAttachment) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("text", text); err != nil {
		t.Fatal(err)
	}
	if intent != "" {
		if err := writer.WriteField("intent", intent); err != nil {
			t.Fatal(err)
		}
	}
	for _, attachment := range attachments {
		part, err := writer.CreateFormFile("attachment", attachment.Filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(attachment.Contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+jobID+"/messages", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func controlTestJSON(t *testing.T, response *httptest.ResponseRecorder, status int, output any) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
}

func TestSkillRefreshRejectsUnsupportedProfileBeforeAdmission(t *testing.T) {
	ctx := context.Background()
	store, tasks, name := controlTestStore(t)
	profile, err := store.SandboxProfile(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	profile.Name += "-pi"
	profile.Harness = "pi"
	profile, _, err = store.CreateSandboxProfile(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, profile.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, verification, "pi-test"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, verification); err != nil {
		t.Fatal(err)
	}
	job, _, err := store.AdmitDirect(ctx, core.JobAdmission{AdmissionKey: profile.Name, SandboxProfile: profile.Name, ProviderConnection: "primary", Model: "test-model", ReasoningEffort: "high"}, tasks.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	input := core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "refresh", Input: "continue", Intent: core.MessageAuto, RefreshSkills: true}
	if _, err := (composedMessageAdmissions{store: store}).AdmitAgentMessage(ctx, input); err != controlapi.ErrSkillRefreshUnavailable {
		t.Fatalf("Go refresh rejection=%v", err)
	}
	api := controlAPIJobs{store: store, tasks: tasks}
	if _, _, err := api.SendMessage(ctx, job.ID, "refresh", controlapi.SendMessageRequest{Text: "continue", RefreshSkills: true}); err != controlapi.ErrSkillRefreshUnavailable {
		t.Fatalf("HTTP refresh rejection=%v", err)
	}
	messages, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("unsupported refresh was retained: messages=%v err=%v", messages, err)
	}
	input.RefreshSkills = false
	admitted, err := (composedMessageAdmissions{store: store}).AdmitAgentMessage(ctx, input)
	if err != nil || !admitted.Created {
		t.Fatalf("ordinary Pi message=%+v err=%v", admitted, err)
	}
}
