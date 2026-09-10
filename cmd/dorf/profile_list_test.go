package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/clientconfig"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlclient"
)

func TestRemoteProfileListUsesConnectedAPIWithoutHostConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("DORF_GITHUB_API_URL", "deliberately-invalid-host-configuration")
	if err := clientconfig.Save(clientconfig.Path(root), clientconfig.Config{
		DeploymentURL: "https://dorf.example.test", Credential: "test-client",
	}); err != nil {
		t.Fatal(err)
	}
	const payload = `{"profiles":[{"name":"cloud","provider":"e2b","harness":"codex","default":true,"verified":true},{"name":"local","provider":"incus","harness":"pi","default":false,"verified":false}]}`
	body := payload
	requests := 0
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != "GET" || request.URL.String() != "https://dorf.example.test/v1/profiles" || request.Header.Get("Authorization") != "Bearer test-client" {
			t.Fatalf("unexpected profile request: %s %s", request.Method, request.URL)
		}
		response := httptest.NewRecorder()
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.WriteString(body)
		return response.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var human, structured strings.Builder
	for _, output := range []struct {
		args []string
		out  *strings.Builder
	}{
		{[]string{"profile", "list"}, &human},
		{[]string{"profile", "list", "--output", "json"}, &structured},
	} {
		if err := run(context.Background(), output.args, output.out, &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
	}
	if human.String() != "cloud  provider=e2b  harness=codex  default=true  verified=true\nlocal  provider=incus  harness=pi  default=false  verified=false\n" {
		t.Fatalf("human profiles=%q", human.String())
	}
	var got, want controlapi.ProfileList
	if err := json.Unmarshal([]byte(structured.String()), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON profiles=%+v, want %+v", got, want)
	}
	for _, args := range [][]string{{"profile", "list", "cloud"}, {"profile", "list", "--output", "yaml"}} {
		if err := run(context.Background(), args, &strings.Builder{}, &strings.Builder{}); err == nil {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
	if requests != 2 {
		t.Fatalf("invalid arguments reached API; requests=%d", requests)
	}
	body = `{"profiles":[]}`
	var empty strings.Builder
	if err := run(context.Background(), []string{"profile", "list"}, &empty, &strings.Builder{}); err != nil || empty.String() != "No Sandbox profiles found\n" {
		t.Fatalf("empty profiles=%q err=%v", empty.String(), err)
	}
}

func TestUnknownProfileErrorGuidesRemoteAndHostClients(t *testing.T) {
	problem, ok := controlapi.ProblemForCode("profile_not_found")
	if !ok {
		t.Fatal("missing profile_not_found Problem")
	}
	cause := &controlclient.ProblemError{Problem: problem}
	for _, deployment := range []string{"https://dorf.example.test", "http://127.0.0.1:8745"} {
		err := jobControlError(deployment, cause)
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "dorf profile list") {
			t.Fatalf("profile error for %s=%v", deployment, err)
		}
	}
}
