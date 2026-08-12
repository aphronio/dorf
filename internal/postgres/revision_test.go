package postgres

import (
	"strings"
	"testing"
)

func TestValidRevisionRequiresAFullImmutableCommitOID(t *testing.T) {
	for _, value := range []string{strings.Repeat("a", 40), strings.Repeat("b", 64)} {
		if !ValidRevision(value) {
			t.Fatalf("rejected full commit OID %q", value)
		}
	}
	for _, value := range []string{"2d2e0fb", "main", strings.Repeat("g", 40), strings.Repeat("A", 40), strings.Repeat("a", 41)} {
		if ValidRevision(value) {
			t.Fatalf("accepted mutable or abbreviated revision %q", value)
		}
	}
}
