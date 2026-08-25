package release

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type updateTransport func(*http.Request) (*http.Response, error)

func (transport updateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestApplicationUpdaterRunsVerifiedLatestInstallerBesideCurrentExecutable(t *testing.T) {
	installer := []byte("verified installer\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(installer))
	tag := "v1.2.4"
	assetURL := "https://github.com/aphronio/dorf/releases/download/" + tag + "/" + installerName
	client := &http.Client{Transport: updateTransport(func(request *http.Request) (*http.Response, error) {
		var body string
		switch request.URL.String() {
		case "https://api.example/latest":
			body = fmt.Sprintf(`{"tag_name":%q,"immutable":true,"assets":[{"name":%q,"size":%d,"digest":%q,"browser_download_url":%q}]}`, tag, installerName, len(installer), "sha256:"+digest, assetURL)
		case assetURL:
			body = string(installer)
		default:
			t.Fatalf("unexpected request %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	executable := filepath.Join(t.TempDir(), "bin", "dorf")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := false
	updater := applicationUpdater{
		client: client, apiURL: "https://api.example/latest", currentVersion: "1.2.3",
		executable: func() (string, error) { return executable, nil },
		runInstaller: func(_ context.Context, path, installDir, gotTag string, _, _ io.Writer) error {
			run = true
			contents, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(contents) != string(installer) || installDir != filepath.Dir(executable) || gotTag != tag {
				t.Fatalf("installer=%q dir=%q tag=%q", contents, installDir, gotTag)
			}
			return nil
		},
	}
	result, err := updater.update(context.Background(), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !run || !result.Updated || result.From != "1.2.3" || result.Latest != "1.2.4" {
		t.Fatalf("run=%v result=%#v", run, result)
	}
}

func TestApplicationUpdaterDoesNotDowngradeOrReinstall(t *testing.T) {
	for _, latest := range []string{"v1.2.3", "v1.2.2"} {
		t.Run(latest, func(t *testing.T) {
			client := &http.Client{Transport: updateTransport(func(request *http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"tag_name":%q,"immutable":true}`, latest)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			updater := applicationUpdater{
				client: client, apiURL: "https://api.example/latest", currentVersion: "1.2.3",
				executable: func() (string, error) { t.Fatal("resolved executable for a no-op update"); return "", nil },
				runInstaller: func(context.Context, string, string, string, io.Writer, io.Writer) error {
					t.Fatal("ran installer for a no-op update")
					return nil
				},
			}
			result, err := updater.update(context.Background(), io.Discard, io.Discard)
			if err != nil || result.Updated || result.Latest != strings.TrimPrefix(latest, "v") {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestApplicationUpdaterRejectsUntrustedReleaseOrInstaller(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		asset    string
		want     string
	}{
		{name: "mutable", metadata: `{"tag_name":"v1.2.4","immutable":false}`, want: "not an immutable"},
		{
			name:     "tampered installer",
			metadata: `{"tag_name":"v1.2.4","immutable":true,"assets":[{"name":"install.sh","size":5,"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","browser_download_url":"https://github.com/aphronio/dorf/releases/download/v1.2.4/install.sh"}]}`,
			asset:    "wrong", want: "does not match",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: updateTransport(func(request *http.Request) (*http.Response, error) {
				body := test.metadata
				if request.URL.Host == "github.com" {
					body = test.asset
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			updater := applicationUpdater{client: client, apiURL: "https://api.example/latest", currentVersion: "1.2.3"}
			_, err := updater.update(context.Background(), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestApplicationInstallerIgnoresAuthorityOverrides(t *testing.T) {
	environment := environmentWithout([]string{
		"PATH=/bin", "DORF_RELEASES_URL=https://attacker.example", "DORF_INSTALL_DIR=/tmp/elsewhere",
	}, "DORF_INSTALL_DIR", "DORF_RELEASES_URL")
	if got := strings.Join(environment, "\n"); got != "PATH=/bin" {
		t.Fatalf("filtered environment=%q", got)
	}
}

func TestApplicationInstallerUsesUpdatePresentation(t *testing.T) {
	installer := filepath.Join(t.TempDir(), installerName)
	contents := `#!/bin/sh
update=false
while [ "$#" -gt 0 ]; do
	case "$1" in
		--version | --install-dir) shift 2 ;;
		--update) update=true; shift ;;
		*) exit 2 ;;
	esac
done
printf 'Installed dorf\n'
if [ "$update" = false ]; then
	printf 'Next, initialize Dorf when you are ready:\n  dorf setup\n'
fi
`
	if err := os.WriteFile(installer, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	if err := runApplicationInstaller(context.Background(), installer, t.TempDir(), "v1.2.4", &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if output := stdout.String(); !strings.Contains(output, "Installed dorf") || strings.Contains(output, "dorf setup") {
		t.Fatalf("update installer output=%q", output)
	}
}
