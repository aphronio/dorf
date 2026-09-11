package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type attachmentFlags []string

func (f *attachmentFlags) String() string {
	return strings.Join(*f, ",")
}

func (f *attachmentFlags) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("--attach requires a local file path")
	}
	if len(*f) == core.MaxMessageAttachments {
		return fmt.Errorf("--attach accepts at most %d files", core.MaxMessageAttachments)
	}
	*f = append(*f, value)
	return nil
}

func readMessageText(filename, command string, hasAttachments bool) (string, error) {
	if strings.TrimSpace(filename) == "" {
		if hasAttachments {
			return "", nil
		}
		return "", fmt.Errorf("%s requires a file with complete Message", command)
	}
	contents, err := readBoundedLocalFile(filename, core.MaxMessageInputBytes, "Message")
	if err != nil {
		return "", err
	}
	if !utf8.Valid(contents) || bytes.IndexByte(contents, 0) >= 0 {
		return "", fmt.Errorf("Message file must contain UTF-8 text without NUL bytes")
	}
	if !hasAttachments && strings.TrimSpace(string(contents)) == "" {
		return "", fmt.Errorf("complete Message cannot be empty")
	}
	return string(contents), nil
}

func readMessageInput(filename, command string, paths []string) (controlapi.SendMessageRequest, error) {
	attachments, err := readMessageAttachments(paths)
	if err != nil {
		return controlapi.SendMessageRequest{}, err
	}
	text, err := readMessageText(filename, command, len(attachments) != 0)
	if err != nil {
		return controlapi.SendMessageRequest{}, err
	}
	return controlapi.SendMessageRequest{Text: text, Attachments: attachments}, nil
}

func readMessageAttachments(paths []string) ([]controlapi.SendMessageAttachment, error) {
	attachments := make([]controlapi.SendMessageAttachment, 0, len(paths))
	for _, localPath := range paths {
		contents, err := readBoundedLocalFile(localPath, provider.MaxFileWriteBytes, "attachment")
		if err != nil {
			return nil, err
		}
		filename, err := messageAttachmentFilename(filepath.Base(localPath))
		if err != nil {
			return nil, fmt.Errorf("attachment %q has an invalid filename", localPath)
		}
		attachments = append(attachments, controlapi.SendMessageAttachment{Filename: filename, Contents: contents})
	}
	return attachments, nil
}

func readBoundedLocalFile(filename string, limit int, noun string) ([]byte, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s file must be a regular file", noun)
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("%s file exceeds %s", noun, byteLimitName(limit))
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(contents) > limit {
		return nil, fmt.Errorf("%s file exceeds %s", noun, byteLimitName(limit))
	}
	return contents, nil
}

func byteLimitName(limit int) string {
	if limit%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", limit/(1<<20))
	}
	return fmt.Sprintf("%d bytes", limit)
}
