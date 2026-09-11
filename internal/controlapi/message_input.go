package controlapi

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

const maxMessageMultipartBodyBytes = core.MaxMessageInputBytes + core.MaxMessageAttachments*provider.MaxFileWriteBytes + 64<<10

var errMessageBodyTooLarge = errors.New("Message request body is too large")

func (h *handler) exactMessageRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, problem("method_not_allowed"))
		return false
	}
	if r.URL.RawQuery != "" {
		h.fail(w, problem("invalid_query"))
		return false
	}
	if hasConditionalHeader(r) {
		h.fail(w, problem("unsupported_precondition"))
		return false
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		h.fail(w, problem("unsupported_media_type"))
		return false
	}
	if contentTypes[0] == "application/json" {
		return true
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "multipart/form-data" || parameters["boundary"] == "" {
		h.fail(w, problem("unsupported_media_type"))
		return false
	}
	return true
}

func (h *handler) decodeMessage(w http.ResponseWriter, r *http.Request, output *SendMessageRequest) bool {
	if r.Header.Get("Content-Type") == "application/json" {
		return h.decode(w, r, output)
	}
	if r.ContentLength > maxMessageMultipartBodyBytes {
		h.fail(w, problem("body_too_large"))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageMultipartBodyBytes)
	if err := decodeMessageMultipart(r, output); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.Is(err, errMessageBodyTooLarge) || errors.As(err, &tooLarge) {
			h.fail(w, problem("body_too_large"))
		} else {
			h.fail(w, problem("invalid_input"))
		}
		return false
	}
	return true
}

func decodeMessageMultipart(r *http.Request, output *SendMessageRequest) error {
	reader, err := r.MultipartReader()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := decodeMessagePart(part, output, seen); err != nil {
			return err
		}
	}
	if !seen["text"] || len(output.Attachments) == 0 {
		return fmt.Errorf("multipart Message requires text and attachments")
	}
	return nil
}

func decodeMessagePart(part *multipart.Part, output *SendMessageRequest, seen map[string]bool) error {
	if part.FormName() == "attachment" {
		return decodeMessageAttachmentPart(part, output)
	}
	return decodeMessageScalarPart(part, output, seen)
}

func decodeMessageAttachmentPart(part *multipart.Part, output *SendMessageRequest) error {
	filename := part.FileName()
	if len(output.Attachments) == core.MaxMessageAttachments || filename == "" {
		part.Close()
		return fmt.Errorf("invalid attachment field")
	}
	contents, err := readMessagePart(part, provider.MaxFileWriteBytes)
	if err != nil {
		return err
	}
	output.Attachments = append(output.Attachments, SendMessageAttachment{Filename: filename, Contents: contents})
	return nil
}

func decodeMessageScalarPart(part *multipart.Part, output *SendMessageRequest, seen map[string]bool) error {
	name := part.FormName()
	if name == "" || part.FileName() != "" || seen[name] {
		part.Close()
		return fmt.Errorf("invalid Message field")
	}
	seen[name] = true
	limit := int64(32)
	if name == "text" {
		limit = core.MaxMessageInputBytes
	}
	contents, err := readMessagePart(part, limit)
	if err != nil {
		return err
	}
	return setMessageScalar(output, name, string(contents))
}

func readMessagePart(part *multipart.Part, limit int64) ([]byte, error) {
	contents, readErr := io.ReadAll(io.LimitReader(part, limit+1))
	closeErr := part.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(contents)) > limit {
		return nil, errMessageBodyTooLarge
	}
	return contents, nil
}

func setMessageScalar(output *SendMessageRequest, name, value string) error {
	switch name {
	case "text":
		output.Text = value
	case "intent":
		output.Intent = value
	case "refresh_skills":
		if value != "true" && value != "false" {
			return fmt.Errorf("refresh_skills must be true or false")
		}
		output.RefreshSkills = value == "true"
	default:
		return fmt.Errorf("unknown Message field %q", name)
	}
	return nil
}
