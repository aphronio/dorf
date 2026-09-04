package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aphronio/dorf/internal/decisionindex"
)

func main() {
	root := flag.String("root", ".", "Dorf repository root")
	check := flag.Bool("check", false, "fail when the generated decision indexes are stale")
	flag.Parse()
	if err := run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root string, check bool) error {
	sourceDir := filepath.Join(root, "docs", "project", "decisions")
	current, historical, err := decisionindex.Source(sourceDir)
	if err != nil {
		return err
	}
	outputs := []struct {
		path    string
		content []byte
	}{
		{path: filepath.Join(root, "docs", "project", "decisions.md"), content: current},
		{path: filepath.Join(sourceDir, "archive.md"), content: historical},
	}
	for _, output := range outputs {
		if err := writeOrCheck(output.path, output.content, check); err != nil {
			return err
		}
	}
	return nil
}

func writeOrCheck(path string, generated []byte, check bool) error {
	if !check {
		if err := os.WriteFile(path, generated, 0o644); err != nil {
			return fmt.Errorf("write generated decision index %s: %w", path, err)
		}
		return nil
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read generated decision index %s: %w", path, err)
	}
	if !bytes.Equal(current, generated) {
		return fmt.Errorf("generated decision indexes are stale; run mise run decisions:generate")
	}
	return nil
}
