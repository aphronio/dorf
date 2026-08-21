package brand

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSetupBannerOwnsOneStablePlainAndColoredLockup(t *testing.T) {
	wantWordmark := []string{
		"      ██                     █████",
		"      ██                    ██    ",
		" ███████  ███████  ██████  ██████ ",
		"██    ██ ███   ███ ███      ██    ",
		"██    ██ ███   ███ ██       ██    ",
		" ███████  ███████  ██       ██    ",
	}
	if strings.Join(terminalWordmark, "\n") != strings.Join(wantWordmark, "\n") {
		t.Fatalf("terminal wordmark drifted from the approved cover-derived shape:\n%s", strings.Join(terminalWordmark, "\n"))
	}

	plain := SetupBanner(false)
	if strings.Contains(plain, "\x1b[") {
		t.Fatalf("plain banner contains terminal control sequences:\n%s", plain)
	}
	lines := strings.Split(plain, "\n")
	if len(lines) != 9 {
		t.Fatalf("banner lines=%d:\n%s", len(lines), plain)
	}
	wantPadding := strings.Repeat(" ", (53-utf8.RuneCountInString(Tagline))/2)
	if lines[len(lines)-1] != wantPadding+Tagline {
		t.Fatalf("tagline is not centered beneath the complete lockup: %q", lines[len(lines)-1])
	}
	for index, line := range lines[:len(lines)-1] {
		if width := utf8.RuneCountInString(line); width > 53 {
			t.Fatalf("line %d width=%d exceeds lockup width:\n%s", index, width, plain)
		}
	}
	if colored := SetupBanner(true); !strings.Contains(colored, "\x1b[38;2;") || !strings.Contains(colored, Tagline) {
		t.Fatalf("colored banner omitted color or tagline:\n%s", colored)
	}
}
