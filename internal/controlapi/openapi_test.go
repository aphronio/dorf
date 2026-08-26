package controlapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
)

func TestOpenAPIDocumentDescribesTheCompleteRemoteBoundary(t *testing.T) {
	t.Parallel()

	discovery := controlapi.Discovery{
		Product: "dorf", Version: "test", Capabilities: []string{controlapi.OpenAPICapability},
		Links: controlapi.OpenAPIDiscoveryLinks(),
	}
	server := controlapi.NewServer(discovery, nil, nil)
	publication := httptest.NewRecorder()
	server.Handler.ServeHTTP(publication, httptest.NewRequest(http.MethodGet, controlapi.OpenAPIPath, nil))
	if publication.Code != http.StatusOK || publication.Header().Get("Content-Type") != "application/json" || !bytes.Equal(publication.Body.Bytes(), controlapi.OpenAPIDocument()) {
		t.Fatalf("OpenAPI publication status/type/body=%d/%q/%d bytes", publication.Code, publication.Header().Get("Content-Type"), publication.Body.Len())
	}
	discovered := httptest.NewRecorder()
	server.Handler.ServeHTTP(discovered, httptest.NewRequest(http.MethodGet, "/v1", nil))
	var actualDiscovery controlapi.Discovery
	if err := json.Unmarshal(discovered.Body.Bytes(), &actualDiscovery); discovered.Code != http.StatusOK || err != nil || !reflect.DeepEqual(actualDiscovery, discovery) {
		t.Fatalf("discovery status/body=%d/%s, want %#v", discovered.Code, discovered.Body.String(), discovery)
	}

	contents := publication.Body.Bytes()
	var document map[string]any
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("OpenAPIDocument is not JSON: %v", err)
	}
	if document["openapi"] != "3.1.0" {
		t.Fatalf("openapi=%v, want 3.1.0", document["openapi"])
	}

	wantOperations := map[string][]string{
		"/v1":                         {"get"},
		"/v1/openapi.json":            {"get"},
		"/v1/auth/enrollments/redeem": {"post"},
		"/v1/me":                      {"get"},
		"/v1/jobs":                    {"get", "post"},
		"/v1/workflows/coding/jobs":   {"post"},
		"/v1/workflows/codebase-investigation/jobs": {"post"},
		"/v1/jobs/{job}":                    {"get"},
		"/v1/jobs/{job}/watch":              {"get"},
		"/v1/jobs/{job}/messages":           {"post"},
		"/v1/jobs/{job}/messages/{message}": {"get"},
		"/v1/jobs/{job}/retries":            {"post"},
		"/v1/jobs/{job}/evidence":           {"get"},
		"/v1/jobs/{job}/cleanup":            {"put"},
		"/v1/sandboxes/{sandbox}/files":     {"get"},
	}
	paths := objectAt(t, document, "paths")
	if len(paths) != len(wantOperations) {
		t.Fatalf("documented paths=%d, want %d: %#v", len(paths), len(wantOperations), paths)
	}
	operationIDs := make(map[string]bool)
	for route, methods := range wantOperations {
		item := objectAt(t, paths, route)
		if len(item) != len(methods) {
			t.Fatalf("%s methods=%v, want %v", route, mapKeys(item), methods)
		}
		for _, method := range methods {
			operation := objectAt(t, item, method)
			operationID, ok := operation["operationId"].(string)
			if !ok || operationID == "" || operationIDs[operationID] {
				t.Fatalf("%s %s has missing or duplicate operationId %q", method, route, operationID)
			}
			operationIDs[operationID] = true
			responses := objectAt(t, operation, "responses")
			if _, ok := responses["default"]; !ok {
				t.Fatalf("%s %s has no common Problem response", method, route)
			}
		}
	}

	for _, route := range []string{"/v1", "/v1/openapi.json", "/v1/auth/enrollments/redeem"} {
		method := "get"
		if strings.HasSuffix(route, "redeem") {
			method = "post"
		}
		security := arrayAt(t, objectAt(t, objectAt(t, paths, route), method), "security")
		if len(security) != 0 {
			t.Fatalf("%s must be unauthenticated, security=%#v", route, security)
		}
	}
	security := arrayAt(t, document, "security")
	if len(security) != 1 || objectAt(t, security[0])["clientBearer"] == nil {
		t.Fatalf("default security=%#v, want clientBearer", security)
	}

	wantMapping := map[string]any{
		"direct":                 "#/components/schemas/DirectJob",
		"coding":                 "#/components/schemas/CodingJob",
		"codebase-investigation": "#/components/schemas/InvestigationJob",
	}
	mapping := objectAt(t, objectAt(t, objectAt(t, objectAt(t, document, "components"), "schemas"), "Job"), "discriminator", "mapping")
	if !reflect.DeepEqual(mapping, wantMapping) {
		t.Fatalf("Job discriminator=%#v, want %#v", mapping, wantMapping)
	}

	assertRef(t, document, "#/components/schemas/JobList", "paths", "/v1/jobs", "get", "responses", "200", "content", "application/json", "schema", "$ref")
	assertRef(t, document, "#/components/schemas/Job", "paths", "/v1/jobs/{job}/watch", "get", "responses", "200", "content", "text/event-stream", "x-dorf-event", "dataSchema", "$ref")
	assertRef(t, document, "#/components/schemas/Problem", "components", "responses", "Problem", "content", "application/problem+json", "schema", "$ref")
	if _, ok := objectAt(t, document, "paths", "/v1/sandboxes/{sandbox}/files", "get", "responses", "200", "content")["application/octet-stream"]; !ok {
		t.Fatal("Sandbox file response does not describe application/octet-stream")
	}

	var published struct {
		Problems []controlapi.ProblemDescriptor `json:"x-dorf-problems"`
	}
	if err := json.Unmarshal(contents, &published); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(published.Problems, controlapi.ProblemDescriptors()) {
		t.Fatal("published Problem catalog diverges from runtime catalog")
	}

	walkJSON(t, document, document)
	contents[0] = 'x'
	if controlapi.OpenAPIDocument()[0] != '{' {
		t.Fatal("OpenAPIDocument exposed mutable package state")
	}
	if controlapi.OpenAPIDiscoveryLinks().OpenAPI != controlapi.OpenAPIPath || controlapi.OpenAPICapability != "openapi" {
		t.Fatal("OpenAPI discovery metadata is inconsistent")
	}
}

func TestProblemCatalogConstructsThePublishedWireContract(t *testing.T) {
	t.Parallel()

	catalog := controlapi.ProblemDescriptors()
	if len(catalog) == 0 {
		t.Fatal("Problem catalog is empty")
	}
	for i, descriptor := range catalog {
		if i > 0 && catalog[i-1].Code >= descriptor.Code {
			t.Fatalf("Problem catalog is not uniquely code-sorted at %q", descriptor.Code)
		}
		if descriptor.Status < 400 || descriptor.Status > 599 || descriptor.Title == "" || descriptor.Type != "https://dorf.dev/problems/"+strings.ReplaceAll(descriptor.Code, "_", "-") {
			t.Fatalf("invalid descriptor: %#v", descriptor)
		}
		problem, ok := controlapi.ProblemForCode(descriptor.Code)
		if !ok || problem.Type != descriptor.Type || problem.Title != descriptor.Title || problem.Status != descriptor.Status || problem.Code != descriptor.Code || problem.Retryable != descriptor.Retryable || problem.Details == nil || len(problem.Details) != 0 {
			t.Fatalf("ProblemForCode(%q)=%#v,%v, want %#v with empty details", descriptor.Code, problem, ok, descriptor)
		}
	}
	if _, ok := controlapi.ProblemForCode("not_a_dorf_problem"); ok {
		t.Fatal("unknown Problem code was accepted")
	}

	catalog[0].Title = "mutated"
	if controlapi.ProblemDescriptors()[0].Title == "mutated" {
		t.Fatal("ProblemDescriptors exposed mutable package state")
	}
}

func walkJSON(t *testing.T, root, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" {
				reference, ok := child.(string)
				if !ok || resolveReference(root, reference) == nil {
					t.Fatalf("unresolved OpenAPI reference %v", child)
				}
			}
			walkJSON(t, root, child)
		}
	case []any:
		for _, child := range value {
			walkJSON(t, root, child)
		}
	}
}

func resolveReference(root any, reference string) any {
	if !strings.HasPrefix(reference, "#/") {
		return nil
	}
	current := root
	for segment := range strings.SplitSeq(strings.TrimPrefix(reference, "#/"), "/") {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = object[segment]
		if !ok {
			return nil
		}
	}
	return current
}

func assertRef(t *testing.T, document map[string]any, want string, path ...string) {
	t.Helper()
	value := any(document)
	for _, segment := range path {
		value = objectAt(t, value)[segment]
	}
	if value != want {
		t.Fatalf("%s=%v, want %s", strings.Join(path, "."), value, want)
	}
}

func objectAt(t *testing.T, value any, path ...string) map[string]any {
	t.Helper()
	for _, segment := range path {
		object, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s is %T, want object", segment, value)
		}
		value, ok = object[segment]
		if !ok {
			t.Fatalf("object has no %q", segment)
		}
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value is %T, want object", value)
	}
	return object
}

func arrayAt(t *testing.T, value any, path ...string) []any {
	t.Helper()
	for _, segment := range path {
		value = objectAt(t, value)[segment]
	}
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("value is %T, want array", value)
	}
	return array
}

func mapKeys(value map[string]any) []string {
	result := make([]string, 0, len(value))
	for key := range value {
		result = append(result, key)
	}
	return result
}
