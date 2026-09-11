package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestSendMessagePreservesMultipartBytesNamesOrderAndTextOnlyJSON(t *testing.T) {
	files := []controlapi.SendMessageAttachment{
		{Filename: "screen пример.png", Contents: []byte{137, 'P', 'N', 'G', 0, 255}},
		{Filename: `report "final".csv`, Contents: []byte("name,value\nalpha,7\n")},
	}
	for _, input := range []controlapi.SendMessageRequest{
		{Text: " unchanged text\n", Intent: "follow"},
		{Attachments: files},
		{Text: " inspect\n", Intent: "steer", RefreshSkills: true, Attachments: files},
	} {
		t.Run(input.Intent, func(t *testing.T) {
			requests := 0
			client, err := New("https://dorf.example.test", "credential", roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				if request.Method != http.MethodPost || request.URL.Path != "/v1/jobs/worker/messages" || request.Header.Get("Idempotency-Key") != "stable-key" {
					t.Fatalf("incorrect Message identity: %s %s", request.Method, request.URL.Path)
				}
				if len(input.Attachments) == 0 {
					var decoded controlapi.SendMessageRequest
					if request.Header.Get("Content-Type") != "application/json" {
						t.Fatal("text-only request stopped using JSON")
					}
					if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil || !reflect.DeepEqual(decoded, input) {
						t.Fatalf("JSON input=%+v, error=%v", decoded, err)
					}
				} else {
					assertMessageMultipart(t, request, input)
				}
				return jsonResponse(http.StatusCreated, `{"id":"message","job_id":"worker","intent":"follow"}`), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			message, err := client.SendMessage(context.Background(), "worker", "stable-key", input)
			if err != nil || requests != 1 || message.ID != "message" {
				t.Fatalf("receipt=%+v requests=%d error=%v", message, requests, err)
			}
		})
	}
}

func assertMessageMultipart(t *testing.T, request *http.Request, input controlapi.SendMessageRequest) {
	t.Helper()
	reader, err := request.MultipartReader()
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{}
	var attachments []controlapi.SendMessageAttachment
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		contents, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == "attachment" {
			attachments = append(attachments, controlapi.SendMessageAttachment{Filename: part.FileName(), Contents: contents})
		} else {
			fields[part.FormName()] = string(contents)
		}
	}
	if fields["text"] != input.Text || fields["intent"] != input.Intent || (fields["refresh_skills"] == "true") != input.RefreshSkills || !reflect.DeepEqual(attachments, input.Attachments) {
		t.Fatalf("multipart did not preserve the complete ordered input: fields=%+v, attachment count=%d", fields, len(attachments))
	}
}

func TestSendMessageRejectsInvalidAttachmentsBeforeTransport(t *testing.T) {
	for name, files := range map[string][]controlapi.SendMessageAttachment{
		"header injection": {{Filename: "image\r\nInjected: value", Contents: []byte("image")}},
		"invalid UTF-8":    {{Filename: "\xff.png", Contents: []byte("image")}},
		"oversize":         {{Filename: "big.bin", Contents: bytes.Repeat([]byte{1}, provider.MaxFileWriteBytes+1)}},
		"too many":         make([]controlapi.SendMessageAttachment, core.MaxMessageAttachments+1),
	} {
		t.Run(name, func(t *testing.T) {
			client, err := New("https://dorf.example.test", "credential", roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid attachment reached transport")
				return nil, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.SendMessage(context.Background(), "job", "key", controlapi.SendMessageRequest{Attachments: files}); err == nil || strings.Contains(err.Error(), "Injected") {
				t.Fatalf("invalid input error=%v", err)
			}
		})
	}
}
