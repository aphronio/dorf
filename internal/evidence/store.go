package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Blob struct {
	Digest   string
	ByteSize int64
}

type Store struct{ Root string }

func (s Store) Put(contents []byte) (Blob, error) {
	digestBytes := sha256.Sum256(contents)
	digest := hex.EncodeToString(digestBytes[:])
	path := s.path(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Blob{}, err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if sha256.Sum256(existing) != digestBytes {
			return Blob{}, fmt.Errorf("content-addressed Evidence %s does not match its path", digest)
		}
		return Blob{Digest: digest, ByteSize: int64(len(existing))}, nil
	} else if !os.IsNotExist(err) {
		return Blob{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".evidence-*")
	if err != nil {
		return Blob{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return Blob{}, err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return Blob{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return Blob{}, err
	}
	if err := temporary.Close(); err != nil {
		return Blob{}, err
	}
	if err := os.Chmod(temporaryName, 0o444); err != nil {
		return Blob{}, err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return Blob{}, err
		}
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return Blob{}, err
	}
	if err := directory.Sync(); err != nil {
		directory.Close()
		return Blob{}, err
	}
	if err := directory.Close(); err != nil {
		return Blob{}, err
	}
	return Blob{Digest: digest, ByteSize: int64(len(contents))}, nil
}

func (s Store) Verify(digest string, byteSize int64) error {
	decoded, decodeErr := hex.DecodeString(digest)
	if decodeErr != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("invalid Evidence digest %q", digest)
	}
	file, err := os.Open(s.path(digest))
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != digest || size != byteSize {
		return fmt.Errorf("Evidence %s failed independent rehash: digest=%s size=%d expected_size=%d", digest, actual, size, byteSize)
	}
	return nil
}

func (s Store) path(digest string) string {
	if len(digest) < 4 {
		return filepath.Join(s.Root, "invalid")
	}
	return filepath.Join(s.Root, "sha256", digest[:2], digest[2:])
}
