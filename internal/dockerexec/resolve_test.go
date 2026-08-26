package dockerexec

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolveAcceptsOneProtectedFixedCandidate(t *testing.T) {
	for _, want := range []string{SystemPath, LocalPath} {
		t.Run(want, func(t *testing.T) {
			path, err := resolve(fakeLstat(map[string]os.FileInfo{
				want: fakeDockerInfo{mode: 0o755, uid: 0},
			}))
			if err != nil || path != want {
				t.Fatalf("resolve() = %q, %v; want %q", path, err, want)
			}
		})
	}
}

func TestResolveRefusesUnsafeOrAmbiguousCandidates(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]os.FileInfo
		want  string
	}{
		{name: "symlink", files: map[string]os.FileInfo{SystemPath: fakeDockerInfo{mode: os.ModeSymlink | 0o777, uid: 0}}, want: "non-symlink"},
		{name: "non-root", files: map[string]os.FileInfo{SystemPath: fakeDockerInfo{mode: 0o755, uid: 1000}}, want: "root-owned"},
		{name: "missing ownership metadata", files: map[string]os.FileInfo{SystemPath: fakeDockerInfo{mode: 0o755, noOwner: true}}, want: "root-owned"},
		{name: "nonregular", files: map[string]os.FileInfo{SystemPath: fakeDockerInfo{mode: os.ModeDir | 0o755, uid: 0}}, want: "regular file"},
		{name: "not owner executable", files: map[string]os.FileInfo{SystemPath: fakeDockerInfo{mode: 0o644, uid: 0}}, want: "owner-executable"},
		{name: "group writable", files: map[string]os.FileInfo{SystemPath: fakeDockerInfo{mode: 0o775, uid: 0}}, want: "protected"},
		{
			name: "two safe candidates",
			files: map[string]os.FileInfo{
				SystemPath: fakeDockerInfo{mode: 0o755, uid: 0},
				LocalPath:  fakeDockerInfo{mode: 0o755, uid: 0},
			},
			want: "ambiguous",
		},
		{
			name: "unsafe second candidate",
			files: map[string]os.FileInfo{
				SystemPath: fakeDockerInfo{mode: 0o755, uid: 0},
				LocalPath:  fakeDockerInfo{mode: os.ModeSymlink | 0o777, uid: 0},
			},
			want: LocalPath,
		},
		{name: "missing", files: map[string]os.FileInfo{}, want: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if path, err := resolve(fakeLstat(test.files)); err == nil || path != "" || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolve() = %q, %v; want refusal containing %q", path, err, test.want)
			}
		})
	}
}

func TestResolveInspectsEachFixedCandidateOnceWithoutRetry(t *testing.T) {
	calls := map[string]int{}
	denied := errors.New("permission denied")
	path, err := resolve(func(path string) (os.FileInfo, error) {
		calls[path]++
		return nil, denied
	})
	if path != "" || !errors.Is(err, denied) {
		t.Fatalf("resolve() = %q, %v", path, err)
	}
	if calls[SystemPath] != 1 || calls[LocalPath] != 0 {
		t.Fatalf("inspection calls = %#v; resolver retried or continued after unsafe inspection", calls)
	}
}

func fakeLstat(files map[string]os.FileInfo) func(string) (os.FileInfo, error) {
	return func(path string) (os.FileInfo, error) {
		if info, ok := files[path]; ok {
			return info, nil
		}
		return nil, &os.PathError{Op: "lstat", Path: path, Err: os.ErrNotExist}
	}
}

type fakeDockerInfo struct {
	mode    os.FileMode
	uid     uint32
	noOwner bool
}

func (info fakeDockerInfo) Name() string       { return "docker" }
func (info fakeDockerInfo) Size() int64        { return 1 }
func (info fakeDockerInfo) Mode() os.FileMode  { return info.mode }
func (info fakeDockerInfo) ModTime() time.Time { return time.Time{} }
func (info fakeDockerInfo) IsDir() bool        { return info.mode.IsDir() }
func (info fakeDockerInfo) Sys() any {
	if info.noOwner {
		return nil
	}
	return &syscall.Stat_t{Uid: info.uid}
}
