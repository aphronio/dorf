package evidence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentAddressedEvidenceIsImmutableAndIndependentlyRehashed(t *testing.T) {
	store := Store{Root: t.TempDir()}
	contents := []byte("observed command outcome\n")
	first, err := store.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first.Digest) != 64 || first.ByteSize != int64(len(contents)) {
		t.Fatalf("content identity first=%#v second=%#v", first, second)
	}
	if err := store.Verify(first.Digest, first.ByteSize); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root, "sha256", first.Digest[:2], first.Digest[2:])
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("altered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(first.Digest, first.ByteSize); err == nil || !strings.Contains(err.Error(), "failed independent rehash") {
		t.Fatalf("altered Evidence verification=%v", err)
	}
}
