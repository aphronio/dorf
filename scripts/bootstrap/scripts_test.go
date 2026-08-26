package bootstrap

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestBootstrapHelpersAreValidPOSIXShell(t *testing.T) {
	for name, script := range map[string][]byte{
		"docker.sh": dockerScript,
		"incus.sh":  incusScript,
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

func TestDockerHelperMakesAuthorityAndRefusalBoundaryExplicit(t *testing.T) {
	script := string(dockerScript)
	assertContainsAll(t, script,
		"--user",
		"--acknowledge-docker-root-authority",
		"--acknowledge-firewall-impact",
		"Ubuntu 24.04",
		"VERSION_CODENAME",
		"amd64",
		"download.docker.com/linux/ubuntu/gpg",
		"9DC858229FC7DD38854AE2D88D81803C0EBFCD88",
		"docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin",
		"systemctl enable --now docker.service",
		"usermod -aG docker",
		"/usr/bin/docker",
		"/usr/local/bin/docker",
		"DOCKER_EXECUTABLE",
		"/usr/bin/stat -c '%u:%a'",
		"as_user \"$DOCKER_EXECUTABLE\"",
		"context show",
		"unix:///var/run/docker.sock",
		"https://docs.docker.com/engine/install/ubuntu/",
		"https://docs.docker.com/engine/install/linux-postinstall/",
		"https://docs.docker.com/compose/install/linux/",
	)
	assertContainsAll(t, script,
		"root-equivalent",
		"firewall",
		"re-login",
		"refusing",
	)
	for _, forbidden := range []string{
		"if command -v docker",
		"as_user docker",
		"until docker_ready",
		"while ! docker_ready",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("Docker helper contains unpinned or retrying readiness operation %q", forbidden)
		}
	}
	assertForbiddenShellPatterns(t, script)
}

func TestIncusHelperMakesAuthorityAndPristineInitBoundaryExplicit(t *testing.T) {
	script := string(incusScript)
	assertContainsAll(t, script,
		"--user",
		"--acknowledge-incus-root-authority",
		"--acknowledge-kvm-device-access",
		"--initialize-pristine",
		"Ubuntu 24.04",
		"/dev/kvm",
		"https://pkgs.zabbly.com/incus/stable",
		"4EFC590696CB15B87C73A3AD82CC8797C838DCFD",
		"incus qemu-system-x86",
		"systemctl enable --now incus.service",
		"usermod -aG incus-admin",
		"usermod -aG kvm",
		"instance_port_forward",
		"--force-local",
		"incus admin init --minimal",
		"all-projects=true",
		"storage show default",
		"/1.0/networks/incusbr0",
		"project create dorf",
		"restricted=true",
		"features.images=true",
		"features.networks=false",
		"features.profiles=true",
		"features.storage.volumes=true",
		"restricted.networks.access=incusbr0",
		"restricted.storage-pools.access=default",
		"/var/snap/lxd",
		"https://linuxcontainers.org/incus/docs/main/installing/",
		"https://linuxcontainers.org/incus/docs/main/tutorial/first_steps/",
		"https://linuxcontainers.org/incus/docs/main/api-extensions/#instance_port_forward",
		"https://github.com/zabbly/incus",
	)
	assertContainsAll(t, script,
		"root-equivalent",
		"zero storage pools",
		"zero managed networks",
		"partial initialization",
		"refusing",
	)
	for _, forbidden := range []string{"incus version", "7.3", "core.https_address", "config unset", "config set"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("Incus helper contains forbidden authority/version operation %q", forbidden)
		}
	}
	assertForbiddenShellPatterns(t, script)
}

func assertContainsAll(t *testing.T, script string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(script, value) {
			t.Errorf("helper is missing required contract text %q", value)
		}
	}
}

func assertForbiddenShellPatterns(t *testing.T, script string) {
	t.Helper()
	for _, forbidden := range []string{"sudo ", "apt-get remove", "apt remove", "docker pull", "docker load", "dorf setup", "dorf service", "provider-gateway"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("helper contains forbidden operation %q", forbidden)
		}
	}
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`curl[^\n]*\|`),
		regexp.MustCompile(`rm\s+-[^\n]*r`),
	} {
		if pattern.MatchString(script) {
			t.Errorf("helper contains forbidden shell pattern %q", pattern)
		}
	}
}
