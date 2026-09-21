package terminal

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestNativeInputWritesAllTenFilesAndPreservesImages(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	event := core.NativeEvent{Type: core.InputMessage, ClientID: "ten-files", Text: "inspect files"}
	for i := range 10 {
		// Repeated original names still receive distinct ordinal paths.
		event.Attachments = append(event.Attachments, core.NativeAttachment{Filename: "image.png", Contents: append(bytes.Clone(encoded.Bytes()), byte(i))})
	}
	sandbox := &nativeFileSandbox{root: t.TempDir()}
	input, err := (Externals{Sandbox: sandbox}).nativeInput(context.Background(), provider.Ownership{}, event)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Images) != 10 || len(sandbox.paths) != 10 {
		t.Fatalf("images=%d files=%d", len(input.Images), len(sandbox.paths))
	}
	for i, name := range sandbox.paths {
		contents, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(contents, event.Attachments[i].Contents) {
			t.Fatalf("file %d bytes changed: %v", i, err)
		}
		if filepath.Base(filepath.Dir(name)) != fmt.Sprintf("%02d", i+1) || !strings.Contains(input.Text, fmt.Sprintf("%q", name)) {
			t.Fatalf("file %d is missing its ordered local path", i)
		}
		if input.Images[i].MediaType != "image/png" || !bytes.Equal(input.Images[i].Bytes, contents) {
			t.Fatalf("native image %d changed", i)
		}
	}
}

type nativeFileSandbox struct {
	provider.Sandbox
	root  string
	paths []string
}

func (s *nativeFileSandbox) Workspace() string { return s.root }
func (s *nativeFileSandbox) Exec(ctx context.Context, _ provider.Ownership, stdin []byte, args ...string) (provider.Result, error) {
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Stdin = bytes.NewReader(stdin)
	output, err := command.CombinedOutput()
	if err != nil {
		return provider.Result{}, fmt.Errorf("file write: %w: %s", err, output)
	}
	s.paths = append(s.paths, args[5])
	return provider.Result{}, nil
}
