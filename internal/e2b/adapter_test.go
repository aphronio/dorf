package e2b

import (
	"errors"
	"testing"
)

func TestProviderExecResultPropagatesAbnormalExitError(t *testing.T) {
	abnormal := &ExitError{Result: ExecResult{ExitCode: 0, Exited: false, RemoteError: "stream ended"}}
	result, err := providerExecResult(abnormal.Result, "dHJ1bmNhdGVk", "", abnormal)
	if !errors.Is(err, abnormal) || result.Stdout != "dHJ1bmNhdGVk" {
		t.Fatalf("abnormal result=%#v err=%v", result, err)
	}
	ordinary := &ExitError{Result: ExecResult{ExitCode: 7, Exited: true}}
	result, err = providerExecResult(ordinary.Result, "", "failed", ordinary)
	if err != nil || result.ExitCode != 7 || result.Stderr != "failed" {
		t.Fatalf("ordinary result=%#v err=%v", result, err)
	}
	reported := &ExitError{Result: ExecResult{ExitCode: 128, Exited: true, RemoteError: "exit status 128"}}
	result, err = providerExecResult(reported.Result, "", "not a Git repository", reported)
	if err != nil || result.ExitCode != 128 || result.Stderr != "not a Git repository" {
		t.Fatalf("reported ordinary result=%#v err=%v", result, err)
	}
}
