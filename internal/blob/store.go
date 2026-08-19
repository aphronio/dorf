package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

type Ref struct {
	Digest   string
	ByteSize int64
}

type Store struct{ Root string }

func (s Store) Put(contents []byte) (Ref, error) {
	digestBytes := sha256.Sum256(contents)
	digest := hex.EncodeToString(digestBytes[:])
	path := s.path(digest)
	if err := mkdirAllDurable(filepath.Dir(path), 0o700); err != nil {
		return Ref{}, err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if sha256.Sum256(existing) != digestBytes {
			return Ref{}, fmt.Errorf("content-addressed blob %s does not match its path", digest)
		}
		return Ref{Digest: digest, ByteSize: int64(len(existing))}, nil
	} else if !os.IsNotExist(err) {
		return Ref{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return Ref{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return Ref{}, err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return Ref{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return Ref{}, err
	}
	if err := temporary.Close(); err != nil {
		return Ref{}, err
	}
	if err := os.Chmod(temporaryName, 0o444); err != nil {
		return Ref{}, err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return Ref{}, err
		}
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return Ref{}, err
	}
	return Ref{Digest: digest, ByteSize: int64(len(contents))}, nil
}

func mkdirAllDurable(path string, permission os.FileMode) error {
	var missing []string
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("blob directory %s is not a directory", current)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, current)
		if parent := filepath.Dir(current); parent == current {
			return fmt.Errorf("no existing parent for blob directory %s", path)
		}
	}
	for index := len(missing) - 1; index >= 0; index-- {
		directory := missing[index]
		if err := os.Mkdir(directory, permission); err != nil && !os.IsExist(err) {
			return err
		}
		if err := syncDirectory(filepath.Dir(directory)); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return err
	}
	return directory.Close()
}

func (s Store) Verify(digest string, byteSize int64) error {
	_, err := s.ReadVerified(digest, byteSize)
	return err
}

func (s Store) ReadVerified(digest string, byteSize int64) ([]byte, error) {
	if byteSize < 0 || byteSize == math.MaxInt64 {
		return nil, fmt.Errorf("invalid blob byte size %d", byteSize)
	}
	decoded, decodeErr := hex.DecodeString(digest)
	if decodeErr != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("invalid blob digest %q", digest)
	}
	file, err := os.Open(s.path(digest))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, byteSize+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(contents)
	actual := hex.EncodeToString(sum[:])
	if actual != digest || int64(len(contents)) != byteSize {
		return nil, fmt.Errorf("blob %s failed independent rehash: digest=%s size=%d expected_size=%d", digest, actual, len(contents), byteSize)
	}
	return contents, nil
}

func (s Store) path(digest string) string {
	if len(digest) < 4 {
		return filepath.Join(s.Root, "invalid")
	}
	return filepath.Join(s.Root, "sha256", digest[:2], digest[2:])
}
