package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func parseSandboxFileGet(args []string) (sandboxID, relativePath, output string, err error) {
	positionals := make([]string, 0, 2)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--":
			positionals = append(positionals, args[index+1:]...)
			index = len(args)
		case argument == "--output":
			index++
			if index == len(args) {
				return "", "", "", fmt.Errorf("sandbox file get --output requires a destination")
			}
			output = args[index]
		case strings.HasPrefix(argument, "--output="):
			output = strings.TrimPrefix(argument, "--output=")
		case strings.HasPrefix(argument, "-"):
			return "", "", "", fmt.Errorf("sandbox file get does not support option %q", argument)
		default:
			positionals = append(positionals, argument)
		}
	}
	if len(positionals) != 2 || strings.TrimSpace(output) == "" {
		return "", "", "", fmt.Errorf("sandbox file get requires SANDBOX_ID RELATIVE_PATH --output DESTINATION")
	}
	sandboxID, relativePath = strings.TrimSpace(positionals[0]), positionals[1]
	if sandboxID == "" {
		return "", "", "", fmt.Errorf("sandbox file get requires an exact Sandbox ID")
	}
	return sandboxID, relativePath, output, nil
}

func downloadSandboxFile(ctx context.Context, sandboxID, relativePath, output string, stdout io.Writer, read func(context.Context, string) ([]byte, error)) error {
	contents, err := read(ctx, relativePath)
	if err != nil {
		return err
	}
	if output == "-" {
		_, err = stdout.Write(contents)
		return err
	}
	if err := writeDownloadedFile(output, contents); err != nil {
		return fmt.Errorf("write Sandbox %s file to %s: %w", sandboxID, output, err)
	}
	return nil
}

func writeDownloadedFile(destination string, contents []byte) error {
	directory, base := filepath.Split(destination)
	if directory == "" {
		directory = "."
	}
	temporary, err := os.CreateTemp(directory, "."+base+".dorf-download-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	written, err := temporary.Write(contents)
	if err != nil {
		return err
	}
	if written != len(contents) {
		return io.ErrShortWrite
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return err
	}
	keep = true
	return nil
}
