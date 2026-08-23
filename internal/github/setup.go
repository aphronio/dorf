package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var ErrCredentialReplacementRequiresApproval = errors.New("replacing the configured GitHub App credentials requires approval")

type SetupInput struct {
	AppID, SourcePrivateKey string
	ReplaceCredentials      bool
}

type credentialBundle struct {
	AppID      string `json:"app_id"`
	PrivateKey string `json:"private_key"`
}

func (c Client) Setup(ctx context.Context, input SetupInput) error {
	if c.Credentials == "" || !filepath.IsAbs(c.Credentials) {
		return fmt.Errorf("GitHub credentials destination must be an absolute path")
	}
	source, err := readProtectedFile(input.SourcePrivateKey, "GitHub App private-key source")
	if err != nil {
		return err
	}
	key, err := canonicalPrivateKey(source)
	if err != nil {
		return err
	}
	if err := c.verifyAppIdentity(ctx, input.AppID, key); err != nil {
		return err
	}
	bundle, _ := json.Marshal(credentialBundle{AppID: input.AppID, PrivateKey: string(key)})
	bundle = append(bundle, '\n')
	current, mode, exists, err := readInstalledFile(c.Credentials)
	if err != nil {
		return err
	}
	if exists && !bytes.Equal(current, bundle) && !input.ReplaceCredentials {
		return ErrCredentialReplacementRequiresApproval
	}
	if bytes.Equal(current, bundle) && mode.Perm() == 0o600 {
		return nil
	}
	return writeProtectedFile(c.Credentials, bundle)
}

func readProtectedFile(path, label string) ([]byte, error) {
	contents, mode, err := readFileSnapshot(path, label)
	if err == nil && mode.Perm()&0o077 != 0 {
		err = fmt.Errorf("%s must have no group or other permissions", label)
	}
	return contents, err
}

func readFileSnapshot(path, label string) ([]byte, os.FileMode, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", label, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s must be a regular file", label)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", label, err)
	}
	return contents, info.Mode(), nil
}

func readInstalledFile(path string) ([]byte, os.FileMode, bool, error) {
	contents, mode, err := readFileSnapshot(path, "GitHub credentials")
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	return contents, mode, err == nil, err
}

func writeProtectedFile(path string, contents []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return fmt.Errorf("GitHub integration destination must be a directory")
	}
	temporary, err := os.CreateTemp(directory, ".dorf-github-credentials-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err = temporary.Write(contents); err == nil {
		err = temporary.Close()
	} else {
		_ = temporary.Close()
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return fmt.Errorf("install GitHub credentials: %w", err)
	}
	return nil
}
