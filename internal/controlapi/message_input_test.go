package controlapi_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestMessageMultipartPreservesTextOptionsAndOrderedAttachmentBytes(t *testing.T) {
	credential := "dcr_multipart-client"
	sessions := &fakeSessions{
		session: controlapi.Session{ID: "job-multipart"},
		message: controlapi.Message{ID: "message-multipart", SessionID: "job-multipart"}, messageCreated: true,
	}
	server := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential, client: controlauth.Client{ID: "client"}}, sessions, nil)
	request := messageMultipartRequest(t, "/v1/sessions/job-multipart/messages", func(writer *multipart.Writer) {
		writeMultipartField(t, writer, "text", "")
		writeMultipartField(t, writer, "intent", "auto")
		writeMultipartField(t, writer, "refresh_skills", "true")
		writeMultipartFile(t, writer, "attachment", "diagram-ä.png", []byte("first bytes"))
		writeMultipartFile(t, writer, "attachment", "notes.txt", []byte("second bytes"))
	})
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Idempotency-Key", "multipart-send")
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status/body=%d/%s", response.Code, response.Body.String())
	}
	want := controlapi.SendMessageRequest{
		Text: "", Intent: "auto", RefreshSkills: true,
		Attachments: []controlapi.SendMessageAttachment{
			{Filename: "diagram-ä.png", Contents: []byte("first bytes")},
			{Filename: "notes.txt", Contents: []byte("second bytes")},
		},
	}
	if sessions.messageKey != "multipart-send" || !reflect.DeepEqual(sessions.messageInput, want) {
		t.Fatalf("key/input=%q/%#v, want multipart-send/%#v", sessions.messageKey, sessions.messageInput, want)
	}
}

func TestMessageMultipartRejectsAmbiguousOrUnboundedPartsBeforeAdmission(t *testing.T) {
	tests := []struct {
		name string
		code string
		add  func(*testing.T, *multipart.Writer)
	}{
		{name: "missing text", code: "invalid_input", add: func(t *testing.T, writer *multipart.Writer) {
			writeMultipartFile(t, writer, "attachment", "file.txt", []byte("bytes"))
		}},
		{name: "missing attachment", code: "invalid_input", add: func(t *testing.T, writer *multipart.Writer) {
			writeMultipartField(t, writer, "text", "text")
		}},
		{name: "duplicate text", code: "invalid_input", add: func(t *testing.T, writer *multipart.Writer) {
			writeMultipartField(t, writer, "text", "one")
			writeMultipartField(t, writer, "text", "two")
			writeMultipartFile(t, writer, "attachment", "file.txt", []byte("bytes"))
		}},
		{name: "unknown scalar", code: "invalid_input", add: func(t *testing.T, writer *multipart.Writer) {
			writeMultipartField(t, writer, "text", "text")
			writeMultipartField(t, writer, "ordinal", "1")
			writeMultipartFile(t, writer, "attachment", "file.txt", []byte("bytes"))
		}},
		{name: "attachment without filename", code: "invalid_input", add: func(t *testing.T, writer *multipart.Writer) {
			writeMultipartField(t, writer, "text", "text")
			writeMultipartField(t, writer, "attachment", "bytes")
		}},
		{name: "too many attachments", code: "invalid_input", add: func(t *testing.T, writer *multipart.Writer) {
			writeMultipartField(t, writer, "text", "text")
			for index := 0; index <= core.MaxMessageAttachments; index++ {
				writeMultipartFile(t, writer, "attachment", "file.txt", []byte{byte(index)})
			}
		}},
		{name: "oversized attachment", code: "body_too_large", add: func(t *testing.T, writer *multipart.Writer) {
			writeMultipartField(t, writer, "text", "text")
			writeMultipartFile(t, writer, "attachment", "file.bin", bytes.Repeat([]byte{'x'}, provider.MaxFileWriteBytes+1))
		}},
		{name: "invalid refresh", code: "invalid_input", add: func(t *testing.T, writer *multipart.Writer) {
			writeMultipartField(t, writer, "text", "text")
			writeMultipartField(t, writer, "refresh_skills", "1")
			writeMultipartFile(t, writer, "attachment", "file.txt", []byte("bytes"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credential := "dcr_invalid-multipart"
			sessions := &fakeSessions{session: controlapi.Session{ID: "session"}}
			server := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil)
			request := messageMultipartRequest(t, "/v1/sessions/session/messages", func(writer *multipart.Writer) { test.add(t, writer) })
			request.Header.Set("Authorization", "Bearer "+credential)
			request.Header.Set("Idempotency-Key", "send")
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, request)
			requireProblem(t, response, controlapiProblemStatus(test.code), test.code)
			if sessions.messageKey != "" {
				t.Fatalf("invalid multipart reached Message admission: %#v", sessions.messageInput)
			}
		})
	}
}

func TestMessageAttachmentAdmissionErrorsHaveStableProblems(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{err: controlapi.ErrAttachmentAnimationUnsupported, code: "attachment_animation_unsupported"},
		{err: controlapi.ErrAttachmentImageTooLarge, code: "attachment_image_too_large"},
		{err: controlapi.ErrMessageImageUnsupported, code: "message_image_unsupported"},
	} {
		t.Run(test.code, func(t *testing.T) {
			credential := "dcr_attachment-problem"
			sessions := &fakeSessions{session: controlapi.Session{ID: "session"}, messageErr: test.err}
			server := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil)
			request := httptest.NewRequest(http.MethodPost, "/v1/sessions/session/messages", strings.NewReader(`{"text":"image"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+credential)
			request.Header.Set("Idempotency-Key", "send")
			response := httptest.NewRecorder()
			server.Handler.ServeHTTP(response, request)
			requireProblem(t, response, http.StatusUnprocessableEntity, test.code)
		})
	}
}

func messageMultipartRequest(t *testing.T, target string, add func(*multipart.Writer)) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	add(writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func writeMultipartField(t *testing.T, writer *multipart.Writer, name, value string) {
	t.Helper()
	if err := writer.WriteField(name, value); err != nil {
		t.Fatal(err)
	}
}

func writeMultipartFile(t *testing.T, writer *multipart.Writer, field, filename string, contents []byte) {
	t.Helper()
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(contents); err != nil {
		t.Fatal(err)
	}
}

func controlapiProblemStatus(code string) int {
	for _, descriptor := range controlapi.ProblemDescriptors() {
		if descriptor.Code == code {
			return descriptor.Status
		}
	}
	return 0
}
