package version

import (
	"regexp"
	"testing"
)

func TestVersionUsesNumericMajorMinorPatchFormat(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(Version) {
		t.Fatalf("Version = %q, want numeric major.minor.patch format", Version)
	}
}
