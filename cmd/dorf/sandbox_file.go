package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aphronio/dorf/internal/core"
)

func sandboxCommand(ctx context.Context, application core.Application, args []string, stdout, _ io.Writer) error {
	if len(args) < 2 || args[0] != "file" || args[1] != "get" {
		return fmt.Errorf("sandbox requires: file get JOB_ID RELATIVE_PATH --output DESTINATION")
	}
	return sandboxFileGet(ctx, application, args[2:], stdout)
}

func sandboxFileGet(ctx context.Context, application core.Application, args []string, stdout io.Writer) error {
	var output, sandboxID string
	var sandboxSet bool
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
				return fmt.Errorf("sandbox file get --output requires a destination")
			}
			output = args[index]
		case strings.HasPrefix(argument, "--output="):
			output = strings.TrimPrefix(argument, "--output=")
		case argument == "--sandbox":
			sandboxSet = true
			index++
			if index == len(args) {
				return fmt.Errorf("sandbox file get --sandbox requires an exact Sandbox ID")
			}
			sandboxID = args[index]
		case strings.HasPrefix(argument, "--sandbox="):
			sandboxSet = true
			sandboxID = strings.TrimPrefix(argument, "--sandbox=")
		case strings.HasPrefix(argument, "-"):
			return fmt.Errorf("sandbox file get does not support option %q", argument)
		default:
			positionals = append(positionals, argument)
		}
	}
	if len(positionals) != 2 || strings.TrimSpace(output) == "" {
		return fmt.Errorf("sandbox file get requires JOB_ID RELATIVE_PATH --output DESTINATION")
	}
	if sandboxSet && strings.TrimSpace(sandboxID) == "" {
		return fmt.Errorf("sandbox file get --sandbox requires an exact Sandbox ID")
	}
	job, err := application.OpenJob(ctx, positionals[0])
	if err != nil {
		return err
	}
	var sandbox core.SandboxHandle
	if sandboxID == "" {
		sandbox, err = job.DefaultSandbox(ctx)
	} else {
		sandbox, err = job.Sandbox(ctx, sandboxID)
	}
	if err != nil {
		return err
	}
	contents, err := sandbox.ReadFile(ctx, positionals[1])
	if err != nil {
		return err
	}
	if output == "-" {
		_, err = stdout.Write(contents)
		return err
	}
	if err := writeDownloadedFile(output, contents); err != nil {
		return fmt.Errorf("write Sandbox file to %s: %w", output, err)
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
