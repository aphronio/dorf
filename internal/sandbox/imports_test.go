package sandbox

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCommonConsumersDoNotImportSandboxProviders(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	for _, directory := range []string{"controlplane", "spine", "workflow", "repository", "publication", "terminal", "codex", "pi"} {
		path := filepath.Join(root, "internal", directory)
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			filename := filepath.Join(path, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range file.Imports {
				name, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(name, "/internal/incus") || strings.HasSuffix(name, "/internal/e2b") {
					t.Errorf("%s imports concrete Sandbox provider %s", filename, name)
				}
			}
		}
	}
}

func TestCoreAndSandboxProvidersDoNotImportWorkflowModules(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	workflowModules := []string{"/internal/coding", "/internal/gitworkspace", "/internal/investigation", "/internal/outcome", "/internal/publication", "/internal/workflow"}
	for _, directory := range []string{"controlplane", "spine", "sandbox", "incus", "e2b"} {
		path := filepath.Join(root, "internal", directory)
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			filename := filepath.Join(path, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range file.Imports {
				name, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				for _, module := range workflowModules {
					if strings.HasSuffix(name, module) {
						t.Errorf("%s imports workflow module %s", filename, name)
					}
				}
			}
		}
	}
}
