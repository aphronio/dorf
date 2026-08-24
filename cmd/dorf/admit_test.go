package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

type admissionGitHub struct {
	calls        *[]string
	installation string
	discoverErr  error
}

func (g admissionGitHub) DiscoverInstallation(_ context.Context, repository string) (string, error) {
	*g.calls = append(*g.calls, "discover:"+repository)
	return g.installation, g.discoverErr
}

type admissionJobs struct {
	calls *[]string
	job   coding.Job
	err   error
}

func (s admissionJobs) CodingJob(_ context.Context, id string) (coding.Job, error) {
	*s.calls = append(*s.calls, "load:"+id)
	return s.job, s.err
}

func TestCodingAdmissionDerivesRepositoryAndDiscoversInstallationBeforeAdmission(t *testing.T) {
	var calls []string
	revision := strings.Repeat("a", 40)
	input := coding.Admission{
		JobAdmission: core.JobAdmission{AdmissionKey: "derived-authority", Goal: "change it"},
		Repository:   "https://github.com/aphronio/dorf.git", Revision: revision, Branch: "dorf/derived-authority", BaseBranch: "main",
	}
	var admitted coding.Admission
	job, created, err := resolveAndAdmitCoding(context.Background(), admissionJobs{calls: &calls, err: postgres.ErrNotFound}, admissionGitHub{
		calls: &calls, installation: "42",
	}, input, func(_ context.Context, resolved coding.Admission) (core.Job, bool, error) {
		calls = append(calls, "admit")
		admitted = resolved
		return core.Job{ID: "job-derived"}, true, nil
	})
	if err != nil || !created || job.ID != "job-derived" {
		t.Fatalf("resolved admission job=%#v created=%t err=%v", job, created, err)
	}
	if strings.Join(calls, ",") != "load:"+core.JobID(input.AdmissionKey)+",discover:aphronio/dorf,admit" {
		t.Fatalf("coding admission order=%v", calls)
	}
	if admitted.GitHubInstallation != "42" || admitted.Revision != revision || admitted.GitHubRepository != "aphronio/dorf" || admitted.BaseBranch != input.BaseBranch {
		t.Fatalf("derived coding admission=%#v", admitted)
	}
}

func TestCodingAdmissionInstallationDiscoveryFailurePreventsAdmission(t *testing.T) {
	var calls []string
	want := errors.New("GitHub installation unavailable")
	_, _, err := resolveAndAdmitCoding(context.Background(), nil, admissionGitHub{
		calls: &calls, discoverErr: want,
	}, coding.Admission{Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("b", 40), BaseBranch: "main"}, func(_ context.Context, _ coding.Admission) (core.Job, bool, error) {
		calls = append(calls, "admit")
		return core.Job{}, false, nil
	})
	if !errors.Is(err, want) || strings.Contains(strings.Join(calls, ","), "admit") {
		t.Fatalf("installation discovery failure calls=%v err=%v", calls, err)
	}
}

func TestCodingAdmissionExactReplayUsesDurableDerivedAuthorityWithoutGitHub(t *testing.T) {
	var calls []string
	revision := strings.Repeat("c", 40)
	input := coding.Admission{
		JobAdmission: core.JobAdmission{AdmissionKey: "lost-response", Goal: "change it"},
		Repository:   "https://github.com/aphronio/dorf.git", Revision: revision, Branch: "dorf/lost-response", BaseBranch: "main",
	}
	githubUnavailable := admissionGitHub{calls: &calls, discoverErr: errors.New("GitHub unavailable")}
	jobs := admissionJobs{calls: &calls, job: coding.Job{
		Job: core.Job{ID: "job-lost-response"}, GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", StartingRevision: revision,
	}}
	job, created, err := resolveAndAdmitCoding(context.Background(), jobs, githubUnavailable, input, func(_ context.Context, resolved coding.Admission) (core.Job, bool, error) {
		calls = append(calls, "admit")
		if resolved.GitHubInstallation != "42" || resolved.Revision != revision || resolved.GitHubRepository != "aphronio/dorf" || resolved.BaseBranch != input.BaseBranch {
			t.Fatalf("replay input=%#v", resolved)
		}
		return core.Job{ID: "job-lost-response"}, false, nil
	})
	if err != nil || created || job.ID != "job-lost-response" || strings.Join(calls, ",") != "load:"+core.JobID(input.AdmissionKey)+",admit" {
		t.Fatalf("durable replay calls=%v job=%#v created=%t err=%v", calls, job, created, err)
	}
}

func TestCodingAdmissionExistingJobLeavesChangedCallerCloneAndBaseForStoreConflict(t *testing.T) {
	var calls []string
	want := errors.New("admission key is already bound to different complete Job input")
	revision := strings.Repeat("d", 40)
	input := coding.Admission{
		JobAdmission: core.JobAdmission{AdmissionKey: "changed-replay", Goal: "change it"},
		Repository:   "https://github.com/aphronio/other.git", Revision: revision,
		Branch: "dorf/changed-replay", BaseBranch: "changed-base",
	}
	_, _, err := resolveAndAdmitCoding(context.Background(), admissionJobs{calls: &calls, job: coding.Job{
		Job: core.Job{ID: "job-changed-replay"}, GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", StartingRevision: strings.Repeat("f", 40),
	}}, admissionGitHub{calls: &calls, discoverErr: errors.New("must not be called")}, input, func(_ context.Context, resolved coding.Admission) (core.Job, bool, error) {
		calls = append(calls, "admit")
		if resolved.Repository != input.Repository || resolved.GitHubRepository != "aphronio/other" || resolved.BaseBranch != "changed-base" || resolved.GitHubInstallation != "42" || resolved.Revision != revision {
			t.Fatalf("existing-Job replay masked caller input: %#v", resolved)
		}
		return core.Job{}, false, want
	})
	if !errors.Is(err, want) || strings.Join(calls, ",") != "load:"+core.JobID(input.AdmissionKey)+",admit" {
		t.Fatalf("changed replay calls=%v err=%v", calls, err)
	}
}
