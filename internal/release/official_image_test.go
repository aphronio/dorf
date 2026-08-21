package release

import "testing"

func TestOfficialImageReleaseIsExact(t *testing.T) {
	if got := OfficialImageRelease(); got != "v0.3.0" {
		t.Fatalf("official Incus image release=%q, want v0.3.0", got)
	}
}
