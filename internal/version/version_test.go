package version

import (
	"regexp"
	"strings"
	"testing"
)

func TestVersionIsReleaseSemanticVersionCore(t *testing.T) {
	parts := strings.Split(Version, ".")
	if len(parts) != 3 {
		t.Fatalf("Version = %q; want exactly three dot-separated parts", Version)
	}

	for i, part := range parts {
		if part == "" {
			t.Fatalf("Version part %d is empty in %q", i+1, Version)
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				t.Fatalf("Version part %d = %q; want numeric characters only", i+1, part)
			}
		}
	}

	tag := "v" + Version
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(tag) {
		t.Fatalf("release tag = %q; want v<major>.<minor>.<patch>", tag)
	}
}
