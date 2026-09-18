package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aphronio/dorf/internal/clientconfig"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlclient"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestMessageCLIForwardsImageAndFileToAnotherJob(t *testing.T) {
	files := cliAttachmentFixtures(t)
	jobs := &attachmentCLIJobs{}
	client := attachmentCLIClient(t, jobs)
	cfg := clientconfig.Config{DeploymentURL: "https://dorf.example.test"}
	parentArgs := []string{"--key", "parent-message", "--attach", files[0], "--attach", files[1], "--output", "json"}
	if err := remoteRun(context.Background(), client, cfg, parentArgs, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(jobs.messages) != 1 || jobs.messages[0].jobID != "direct-job" || jobs.messages[0].key != "parent-message" || jobs.messages[0].input.Text != "" {
		t.Fatalf("attachment-only initial message=%+v", jobs.messages)
	}
	parentInput := jobs.messages[0].input
	if len(parentInput.Attachments) != 2 {
		t.Fatalf("initial message attachment count=%d", len(parentInput.Attachments))
	}
	workspace := t.TempDir()
	forwardArgs := []string{"--key", "forward-to-worker", "--intent", "steer", "--refresh-skills", "--output", "json"}
	for ordinal, received := range parentInput.Attachments {
		original, err := os.ReadFile(files[ordinal])
		if err != nil || received.Filename != filepath.Base(files[ordinal]) || !bytes.Equal(original, received.Contents) {
			t.Fatalf("initial CLI upload changed attachment %d: %v", ordinal, err)
		}
		localPath := filepath.Join(workspace, received.Filename)
		if err := os.WriteFile(localPath, received.Contents, 0o600); err != nil {
			t.Fatal(err)
		}
		forwardArgs = append(forwardArgs, "--attach", localPath)
	}
	forwardArgs = append(forwardArgs, "worker-job")
	if err := remoteMessageSend(context.Background(), cfg, client, forwardArgs, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(jobs.messages) != 2 {
		t.Fatalf("forward message count=%d", len(jobs.messages))
	}
	forwarded := jobs.messages[1]
	if forwarded.jobID != "worker-job" || forwarded.key != "forward-to-worker" || forwarded.input.Intent != "steer" || !forwarded.input.RefreshSkills ||
		!reflect.DeepEqual(forwarded.input.Attachments, parentInput.Attachments) {
		t.Fatalf("forwarding changed the destination, intent, or ordered file bytes: %+v", forwarded)
	}
}

func TestAttachmentCLIRejectsInvalidLocalInputBeforeRemoteEffects(t *testing.T) {
	files := cliAttachmentFixtures(t)
	oversize := filepath.Join(t.TempDir(), "large.bin")
	file, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(provider.MaxFileWriteBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	nulInput := filepath.Join(t.TempDir(), "message.txt")
	if err := os.WriteFile(nulInput, []byte("text\x00hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidArgs := map[string][]string{
		"missing":      {"--attach", filepath.Join(t.TempDir(), "missing")},
		"directory":    {"--attach", t.TempDir()},
		"oversize":     {"--attach", oversize},
		"invalid text": {"--attach", files[0], "--input-file", nulInput},
		"too many":     {"--attach", files[0], "--attach", files[0], "--attach", files[0], "--attach", files[0], "--attach", files[0]},
	}
	for name, args := range invalidArgs {
		t.Run(name, func(t *testing.T) {
			client, err := controlclient.New("https://dorf.example.test", "credential", roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid local input caused a remote effect")
				return nil, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if err := remoteRun(context.Background(), client, clientconfig.Config{}, args, io.Discard, io.Discard); err == nil {
				t.Fatal("run accepted invalid local input")
			}
			if err := remoteMessageSend(context.Background(), clientconfig.Config{}, client, append(append([]string{}, args...), "worker"), io.Discard, io.Discard); err == nil {
				t.Fatal("job message accepted invalid local input")
			}
		})
	}
}

func cliAttachmentFixtures(t *testing.T) []string {
	t.Helper()
	var encoded bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{B: 255, A: 255})
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	files := []string{filepath.Join(root, "screen пример.png"), filepath.Join(root, "report.csv")}
	for index, contents := range [][]byte{encoded.Bytes(), []byte("name,value\nexample,42\n")} {
		if err := os.WriteFile(files[index], contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func attachmentCLIClient(t *testing.T, jobs *attachmentCLIJobs) *controlclient.Client {
	t.Helper()
	auth := &remoteCLIAuth{credential: "credential", client: controlauth.Client{ID: "cli-client", Name: "example client"}}
	handler := controlapi.NewServer(controlapi.Discovery{}, auth, jobs, nil).Handler
	client, err := controlclient.New("https://dorf.example.test", "credential", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Result(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type capturedAttachmentMessage struct {
	jobID, key string
	input      controlapi.SendMessageRequest
}

type attachmentCLIJobs struct {
	controlapi.Jobs
	messages []capturedAttachmentMessage
}

func (*attachmentCLIJobs) AdmitDirect(context.Context, string, string, controlapi.AdmitJobRequest) (controlapi.DirectJob, bool, error) {
	return controlapi.DirectJob{Job: controlapi.Job{ID: "direct-job", Kind: controlapi.JobKindDirect}}, true, nil
}
func (j *attachmentCLIJobs) SendMessage(_ context.Context, jobID, key string, input controlapi.SendMessageRequest) (controlapi.Message, bool, error) {
	j.messages = append(j.messages, capturedAttachmentMessage{jobID: jobID, key: key, input: input})
	return controlapi.Message{ID: "message", JobID: jobID, Intent: input.Intent}, true, nil
}
