package bootstrap

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestBootstrapHelpersAreValidPOSIXShell(t *testing.T) {
	for name, script := range map[string][]byte{
		"docker.sh":       dockerScript,
		"incus.sh":        incusScript,
		"incus-remote.sh": incusRemoteScript,
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(string(script))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("sh -n: %v\n%s", err, output)
			}
		})
	}
}

func TestRemoteIncusHelperKeepsExposureAndTrustNarrow(t *testing.T) {
	script := string(incusRemoteScript)
	for _, required := range []string{
		"--acknowledge-remote-incus-exposure is required",
		"--acknowledge-client-revocation is required",
		"Incus 7.3 or newer is required",
		`"instance_port_forward"`,
		"core.https_address conflicts with the exact Tailscale listener",
		"core.remote_token_expiry 15m",
		`config trust add "$CLIENT_NAME" --restricted --projects dorf`,
		`config trust show "$FINGERPRINT"`,
		`config trust remove "$FINGERPRINT"`,
		"restricted only to project dorf",
		"not an Incus client certificate",
		"--connect-timeout 3 --max-time 5 --insecure",
		"is already absent",
		"CLIENT_NAME=dorf-controller",
		"must have exactly one Tailscale IPv4 address",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("remote Incus helper is missing %q", required)
		}
	}
	for _, forbidden := range []string{"tailscale up", "tailscale serve", "tailscale funnel", "ssh ", "sudo ", "0.0.0.0:8443", "config trust add-certificate"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("remote Incus helper contains out-of-scope operation %q", forbidden)
		}
	}
}

func TestBootstrapHelpersRetainSecurityAndAuthorityChecks(t *testing.T) {
	for _, test := range []struct {
		name     string
		script   string
		required []string
	}{
		{
			name:   "docker.sh",
			script: string(dockerScript),
			required: []string{
				"--acknowledge-docker-root-authority is required",
				"--acknowledge-firewall-impact is required",
				`[ "${ID:-}" = ubuntu ] && [ "${VERSION_ID:-}" = 24.04 ] && [ "${VERSION_CODENAME:-}" = noble ]`,
				`[ "$(dpkg --print-architecture)" = amd64 ]`,
				"9DC858229FC7DD38854AE2D88D81803C0EBFCD88",
				`/usr/bin/stat -c '%u:%a'`,
				"must be protected and root-owned",
				"Docker executable authority is ambiguous",
				"remote Docker authority is not accepted",
				"unix:///var/run/docker.sock",
			},
		},
		{
			name:   "incus.sh",
			script: string(incusScript),
			required: []string{
				"--acknowledge-incus-root-authority is required",
				"--acknowledge-kvm-device-access is required",
				`[ "${ID:-}" = ubuntu ] && [ "${VERSION_ID:-}" = 24.04 ] && [ "${VERSION_CODENAME:-}" = noble ]`,
				`[ "$(dpkg --print-architecture)" = amd64 ]`,
				"4EFC590696CB15B87C73A3AD82CC8797C838DCFD",
				"incus --force-local",
				"zero storage pools and zero managed networks; initialize manually or pass --initialize-pristine",
				"partial initialization: storage and managed networks must both exist",
				"restricted=true",
				"features.images=true",
				"features.networks=false",
				"features.profiles=true",
				"features.storage.volumes=true",
				"restricted.networks.access=incusbr0",
				"restricted.storage-pools.access=default",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, required := range test.required {
				if !strings.Contains(test.script, required) {
					t.Errorf("helper is missing security contract %q", required)
				}
			}
		})
	}
}

func TestDockerHelperDoesNotBypassReadinessAuthority(t *testing.T) {
	for _, forbidden := range []string{"if command -v docker", "as_user docker", "until docker_ready", "while ! docker_ready"} {
		if strings.Contains(string(dockerScript), forbidden) {
			t.Errorf("Docker helper contains unpinned or retrying readiness operation %q", forbidden)
		}
	}
}

func TestIncusHelperDoesNotClaimForbiddenAuthority(t *testing.T) {
	for _, forbidden := range []string{"incus version", "7.3", "core.https_address", "config unset", "config set"} {
		if strings.Contains(string(incusScript), forbidden) {
			t.Fatalf("Incus helper contains forbidden authority/version operation %q", forbidden)
		}
	}
}

func TestBootstrapHelpersExcludeUnsafeAndOutOfScopeOperations(t *testing.T) {
	for name, script := range map[string]string{
		"docker.sh":       string(dockerScript),
		"incus.sh":        string(incusScript),
		"incus-remote.sh": string(incusRemoteScript),
	} {
		t.Run(name, func(t *testing.T) {
			for _, forbidden := range []string{"sudo ", "apt-get remove", "apt remove", "docker pull", "docker load", "dorf setup", "dorf service", "provider-gateway"} {
				if strings.Contains(script, forbidden) {
					t.Errorf("helper contains forbidden operation %q", forbidden)
				}
			}
			for _, pattern := range []*regexp.Regexp{regexp.MustCompile(`curl[^\n]*[^|]\|[^|]`), regexp.MustCompile(`rm\s+-[^\n]*r`)} {
				if pattern.MatchString(script) {
					t.Errorf("helper contains forbidden shell pattern %q", pattern)
				}
			}
		})
	}
}
