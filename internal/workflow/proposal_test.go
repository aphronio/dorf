package workflow

import (
	"strings"
	"testing"

	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/spine"
)

func TestProposalObservationRequiresExactIdentity(t *testing.T) {
	revision := strings.Repeat("a", 40)
	proposal := spine.GitHubProposal{Repository: "aphronio/dorf", BaseBranch: "greenfield", HeadBranch: "dorf/job", Number: 94, URL: "https://github.com/aphronio/dorf/pull/94", ProposedRevision: revision}
	pull := githubapi.PullRequest{Repository: proposal.Repository, Base: proposal.BaseBranch, Head: proposal.HeadBranch, Number: proposal.Number, URL: proposal.URL, HeadSHA: revision}
	if err := validateExactProposal(proposal, pull); err != nil {
		t.Fatal(err)
	}
	pull.HeadSHA = strings.Repeat("b", 40)
	if err := validateExactProposal(proposal, pull); err == nil {
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
	seen := admittedGitHubComments([]spine.MessageView{{Message: spine.Message{
		FromKind: spine.MessageFromHuman,
		FromID:   "github-comment:42",
		Input:    "first admitted text",
	}}})
	if !seen["github-comment:42"] {
		t.Fatal("edited GitHub comment was not recognized as already admitted")
	}
	if seen["github-comment:43"] {
		t.Fatal("unseen GitHub comment was marked admitted")
	}
}
