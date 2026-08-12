package postgres_test

import (
	"os"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestAbsurdSDKAndSchemaShareOneReleaseCommit(t *testing.T) {
	goMod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	modulePin := "github.com/earendil-works/absurd/sdks/go/absurd v" + config.AbsurdVersion
	if !strings.Contains(string(goMod), modulePin) {
		t.Fatalf("go.mod does not pin %s", modulePin)
	}
	if !strings.Contains(postgres.AbsurdSchemaURL, postgres.AbsurdReleaseCommit) {
		t.Fatalf("schema URL %q is not pinned to release commit %s", postgres.AbsurdSchemaURL, postgres.AbsurdReleaseCommit)
	}
	if config.AbsurdVersion != "0.5.0" || postgres.AbsurdReleaseCommit != "550d3b9e6f9382d96178de6ab8c90c7f8edf2227" {
		t.Fatal("verified Absurd release provenance changed without updating the proof")
	}
}
