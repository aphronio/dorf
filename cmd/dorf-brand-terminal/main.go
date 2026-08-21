package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/aphronio/dorf/internal/brandgen"
)

func main() {
	logoPath := flag.String("logo", "assets/logo.png", "canonical Dorf logo PNG")
	coverPath := flag.String("cover", "assets/cover.png", "canonical Dorf cover PNG")
	outputPath := flag.String("output", "internal/brand/terminal_generated.go", "generated Go output")
	check := flag.Bool("check", false, "fail when the generated output is stale")
	flag.Parse()
	if err := run(*logoPath, *coverPath, *outputPath, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(logoPath, coverPath, outputPath string, check bool) error {
	logo, err := os.Open(logoPath)
	if err != nil {
		return fmt.Errorf("open logo: %w", err)
	}
	defer logo.Close()
	cover, err := os.Open(coverPath)
	if err != nil {
		return fmt.Errorf("open cover: %w", err)
	}
	defer cover.Close()
	generated, err := brandgen.Source(logo, cover)
	if err != nil {
		return err
	}
	if check {
		current, err := os.ReadFile(outputPath)
		if err != nil {
			return fmt.Errorf("read generated terminal brand: %w", err)
		}
		if !bytes.Equal(current, generated) {
			return fmt.Errorf("generated terminal brand is stale; run mise run brand:generate")
		}
		return nil
	}
	if err := os.WriteFile(outputPath, generated, 0o644); err != nil {
		return fmt.Errorf("write generated terminal brand: %w", err)
	}
	return nil
}
