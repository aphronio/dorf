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
		"incus-remote.sh prepare --acknowledge-kernel-module-impact",
		"--acknowledge-firewall-impact",
		"--acknowledge-kernel-module-impact is required",
		"--acknowledge-firewall-impact is required",
		"--acknowledge-remote-incus-exposure is required",
		"--acknowledge-client-revocation is required",
		"Incus 7.3 or newer is required",
		`"instance_port_forward"`,
		"core.https_address conflicts with the exact Tailscale listener",
		"core.remote_token_expiry=15M",
		`config trust add "$CLIENT_NAME" --restricted --projects dorf-remote --quiet`,
		`config trust show "$FINGERPRINT"`,
		`config trust remove "$FINGERPRINT"`,
		"restricted only to project dorf-remote",
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
	for _, forbidden := range []string{"tailscale up", "tailscale set", "tailscale serve", "tailscale funnel", "ssh ", "sudo ", "0.0.0.0:8443", "config trust add-certificate", "incusbr0", "--projects dorf "} {
		if strings.Contains(script, forbidden) {
			t.Errorf("remote Incus helper contains out-of-scope operation %q", forbidden)
		}
	}
}

func TestRemoteIncusPrepareOwnsExactIsolationContract(t *testing.T) {
	script := string(incusRemoteScript)
	for _, required := range []string{
		"PROJECT=dorf-remote",
		"NETWORK=dorfbr0",
		"ACL=dorf-egress",
		"BRIDGE_ADDRESS=10.254.254.1/24",
		"BRIDGE_SUBNET=10.254.254.0/24",
		"ipv4.dhcp=true",
		"ipv4.nat=true",
		"ipv6.address=none",
		"security.acls.default.ingress.action=reject",
		"security.acls.default.egress.action=reject",
		"REJECT_DESTINATIONS=0.0.0.0/8,10.0.0.0/8,100.64.0.0/10,127.0.0.0/8,169.254.0.0/16,172.16.0.0/12,192.0.0.0/24,192.0.2.0/24,192.88.99.0/24,192.168.0.0/16,198.18.0.0/15,198.51.100.0/24,203.0.113.0/24,224.0.0.0/4,240.0.0.0/4",
		"destination=0.0.0.0/0",
		"restricted=true",
		"features.images=true",
		"features.networks=false",
		"features.profiles=true",
		"features.storage.volumes=true",
		"features.storage.buckets=false",
		"restricted.networks.access=dorfbr0",
		"restricted.storage-pools.access=default",
		"limits.instances=4",
		"limits.virtual-machines=4",
		"/etc/modules-load.d/dorf-br_netfilter.conf",
		"/usr/sbin/modprobe br_netfilter",
		"ufw allow in on dorfbr0 to any port 67 proto udp",
		"ufw allow in on dorfbr0 to 10.254.254.1 port 53 proto udp",
		"ufw allow in on dorfbr0 to 10.254.254.1 port 53 proto tcp",
		"ufw route deny in on dorfbr0 from 10.254.254.0/24 to 10.0.0.0/8",
		"ufw route deny in on dorfbr0 from 10.254.254.0/24 to 100.64.0.0/10",
		"ufw route deny in on dorfbr0 from 10.254.254.0/24 to 169.254.0.0/16",
		"ufw route deny in on dorfbr0 from 10.254.254.0/24 to 172.16.0.0/12",
		"ufw route deny in on dorfbr0 from 10.254.254.0/24 to 192.168.0.0/16",
		"ufw route allow in on dorfbr0 from 10.254.254.0/24",
		"existing project 'dorf-remote' has incompatible restricted authority",
		"existing network 'dorfbr0' has incompatible isolation config",
		"existing ACL 'dorf-egress' has incompatible egress policy",
		"existing UFW rule for dorfbr0 is outside the prepared contract",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("remote Incus prepare helper is missing %q", required)
		}
	}

	deny := strings.Index(script, `ufw route deny in on dorfbr0 from 10.254.254.0/24 to 10.0.0.0/8`)
	allow := strings.Index(script, `ufw route allow in on dorfbr0 from 10.254.254.0/24`)
	if deny < 0 || allow < 0 || deny >= allow {
		t.Error("remote Incus helper must place defense-in-depth route denies before the route allow")
	}

	prepare := strings.Index(script, "prepare)\n")
	if prepare < 0 {
		t.Fatal("remote Incus helper is missing the prepare action")
	}
	preflight := strings.Index(script[prepare:], "\tpreflight_prepare\n")
	mutation := strings.Index(script[prepare:], "\tensure_module\n")
	if preflight < 0 || mutation < 0 || preflight >= mutation {
		t.Error("remote Incus prepare must preflight every retained resource before its first mutation")
	}

	contract := strings.Index(script, "prepared_contract() {\n")
	offer := strings.Index(script, "offer)\n")
	if contract < 0 || offer < 0 || contract >= offer || !strings.Contains(script[contract:offer], "\tacl_ready || die") {
		t.Error("remote Incus prepared contract must verify the exact privileged ACL")
	}
	prepared, trust := -1, -1
	if offer >= 0 {
		prepared = strings.Index(script[offer:], "\tprepared_contract\n")
		trust = strings.Index(script[offer:], `config trust add "$CLIENT_NAME" --restricted --projects dorf-remote`)
	}
	if prepared < 0 || trust < 0 || prepared >= trust {
		t.Error("remote Incus offer must verify the prepared contract before issuing trust")
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

func TestIncusHelpersTrustDirectProtectedExecutablesNotPATHSpellings(t *testing.T) {
	for name, script := range map[string]string{
		"incus.sh":        string(incusScript),
		"incus-remote.sh": string(incusRemoteScript),
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{
				`[ -f "$1" ] && [ ! -L "$1" ] && [ -x "$1" ]`,
				`/usr/bin/stat -c '%u:%g:%a' "$1"`,
				`= 0:0:755`,
				`/usr/bin/incus --force-local`,
			} {
				if !strings.Contains(script, required) {
					t.Errorf("helper is missing direct executable authority %q", required)
				}
			}
			if strings.Contains(script, `command -v incus 2>/dev/null || true)" = /usr/bin/incus`) {
				t.Error("helper compares a PATH alias spelling instead of validating /usr/bin/incus")
			}
		})
	}

	remote := string(incusRemoteScript)
	for _, required := range []string{
		`reviewed_executable /usr/bin/tailscale`,
		`/usr/bin/tailscale ip -4`,
	} {
		if !strings.Contains(remote, required) {
			t.Errorf("remote helper is missing direct Tailscale authority %q", required)
		}
	}
	if strings.Contains(remote, `command -v tailscale 2>/dev/null || true)" = /usr/bin/tailscale`) {
		t.Error("remote helper compares a PATH alias spelling instead of validating /usr/bin/tailscale")
	}
	if !strings.Contains(string(incusScript), `if command -v incus >/dev/null 2>&1 || [ -e /usr/bin/incus ]`) {
		t.Error("install helper no longer detects a foreign existing Incus command or direct binary")
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
