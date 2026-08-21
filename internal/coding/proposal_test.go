package coding

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	githubapi "github.com/aphronio/dorf/internal/github"
)

func TestProposalObservationRequiresExactIdentity(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := Job{GitHubRepository: "aphronio/dorf", BaseBranch: "greenfield", Branch: "dorf/job"}
	proposal := Proposal{Number: 94, URL: "https://github.com/aphronio/dorf/pull/94", ProposedRevision: revision}
	pull := githubapi.PullRequest{Repository: job.GitHubRepository, Base: job.BaseBranch, Head: job.Branch, Number: proposal.Number, URL: proposal.URL, HeadSHA: revision}
	if err := validateExactProposal(job, proposal, pull); err != nil {
		t.Fatal(err)
	}
	pull.HeadSHA = strings.Repeat("b", 40)
	if err := validateExactProposal(job, proposal, pull); err == nil {
		t.Fatal("accepted a pull request for a different Revision")
	}
}

func TestOnlyRepositoryAuthoritiesSupplyHumanFeedback(t *testing.T) {
	for _, test := range []struct {
		name    string
		comment githubapi.Comment
		trusted bool
	}{
		{"owner", githubapi.Comment{ID: 1, UserType: "User", AuthorAssociation: "OWNER"}, true},
		{"collaborator", githubapi.Comment{ID: 2, UserType: "User", AuthorAssociation: "COLLABORATOR"}, true},
		{"member", githubapi.Comment{ID: 3, UserType: "User", AuthorAssociation: "MEMBER"}, false},
		{"public-user", githubapi.Comment{ID: 4, UserType: "User", AuthorAssociation: "NONE"}, false},
		{"bot-owner", githubapi.Comment{ID: 5, UserType: "Bot", AuthorAssociation: "OWNER"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := trustedHumanComment(test.comment); got != test.trusted {
				t.Fatalf("trusted=%t want %t", got, test.trusted)
			}
		})
	}
}

func TestAdmittedGitHubCommentRemainsImmutableAfterEdit(t *testing.T) {
	admitted := admittedGitHubComments([]MessageRecord{{Message: core.Message{
		FromKind: core.MessageFromHuman,
		FromID:   "github-comment:42",
		Input:    "first admitted text",
	}}})
	if _, ok := admitted["github-comment:42"]; !ok {
		t.Fatal("edited GitHub comment was not recognized as already admitted")
	}
	if _, ok := admitted["github-comment:43"]; ok {
		t.Fatal("unseen GitHub comment was marked admitted")
	}
}

func TestFeedbackReplyNamesExactRevisionAndReconcilesByStableMarker(t *testing.T) {
	revision := strings.Repeat("a", 40)
	source := githubapi.Comment{ID: 42, Login: "aphronio", Body: "Please simplify this boundary.\nKeep it direct."}
	body := feedbackReply("job-7", source, revision)
	if !strings.Contains(body, "`"+revision+"`") {
		t.Fatalf("reply does not name exact Revision: %q", body)
	}
	if !strings.Contains(body, "Regarding feedback from @aphronio:") || !strings.Contains(body, "> Please simplify this boundary.\n> Keep it direct.") {
		t.Fatalf("reply does not visibly quote its source comment: %q", body)
	}
	comments := []githubapi.Comment{{ID: 99, Body: body}}
	if !hasFeedbackReply(comments, "job-7", 42) {
		t.Fatal("existing completion reply was not reconciled")
	}
	if hasFeedbackReply(comments, "job-7", 43) || hasFeedbackReply(comments, "job-8", 42) {
		t.Fatal("completion marker matched a different Job or source comment")
	}
}
