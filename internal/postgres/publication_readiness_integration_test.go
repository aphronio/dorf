package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/blob"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/publication"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

type forbiddenPublicationGitHub struct{ calls int }

func (g *forbiddenPublicationGitHub) called() error {
	g.calls++
	return errors.New("GitHub must not be called before readiness")
}

func (g *forbiddenPublicationGitHub) RemoteHead(context.Context, githubapi.Authority, string) (string, bool, error) {
	return "", false, g.called()
}
func (g *forbiddenPublicationGitHub) PushToken(context.Context, githubapi.Authority) (string, error) {
	return "", g.called()
}
func (g *forbiddenPublicationGitHub) PullRequests(context.Context, githubapi.Authority, string, string) ([]githubapi.PullRequest, error) {
	return nil, g.called()
}
func (g *forbiddenPublicationGitHub) CreatePullRequest(context.Context, githubapi.Authority, string, string, string, string) (githubapi.PullRequest, error) {
	return githubapi.PullRequest{}, g.called()
}
func (g *forbiddenPublicationGitHub) UpdatePullRequest(context.Context, githubapi.Authority, int64, string, string, string) (githubapi.PullRequest, error) {
	return githubapi.PullRequest{}, g.called()
}

type forbiddenPublicationRepository struct{ calls int }

func (r *forbiddenPublicationRepository) Relation(context.Context, spine.Job, string) (string, error) {
	r.calls++
	return "", errors.New("repository must not be called before readiness")
}
func (r *forbiddenPublicationRepository) Push(context.Context, spine.Job, string) error {
	r.calls++
	return errors.New("repository must not be called before readiness")
}

func TestPostgresInvalidEvidenceNeverReachesPushExternals(t *testing.T) {
	_, store, _ := testDatabase(t)
	job, revision, _ := prepareReviewIntegrationJob(t, store, "push-invalid-evidence")
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"docs/readiness.md"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReviewPolicy(context.Background(), spine.ReviewPlanRecord{
		JobID: job.ID, Revision: revision, Facts: facts,
		Plan: policy.ReviewPlan{Decision: "no-review"},
	}); err != nil {
		t.Fatal(err)
	}
	_, push, _, err := store.BeginPublication(context.Background(), job.ID, revision)
	if err != nil {
		t.Fatal(err)
	}
	github := &forbiddenPublicationGitHub{}
	repository := &forbiddenPublicationRepository{}
	service := (publication.Service{Store: store, GitHub: github, Repository: repository, Evidence: blob.Store{Root: t.TempDir()}}).WithClaimCheck(func(context.Context) error { return nil })
	err = service.Push(context.Background(), job.ID, revision)
	if err == nil || !strings.Contains(err.Error(), "publication lost exact-Revision readiness") {
		t.Fatalf("Push readiness error=%v", err)
	}
	if github.calls != 0 || repository.calls != 0 {
		t.Fatalf("invalid Evidence reached externals: GitHub calls=%d repository calls=%d", github.calls, repository.calls)
	}
	storedPush, _, err := store.PublicationActions(context.Background(), job.ID, revision)
	if err != nil || storedPush.ID != push.ID || storedPush.State != spine.ActionUnsettled {
		t.Fatalf("Push Action=%#v err=%v", storedPush, err)
	}
}
