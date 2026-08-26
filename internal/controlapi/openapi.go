package controlapi

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
)

const (
	OpenAPIPath       = "/v1/openapi.json"
	OpenAPICapability = "openapi"
)

// DiscoveryLinks is the stable link shape added to Deployment discovery when
// OpenAPI publication is enabled.
type DiscoveryLinks struct {
	OpenAPI string `json:"openapi"`
}

func OpenAPIDiscoveryLinks() DiscoveryLinks {
	return DiscoveryLinks{OpenAPI: OpenAPIPath}
}

//go:embed openapi.json
var openAPIBase []byte

var publishedOpenAPI = mustPublishOpenAPI()

// OpenAPIDocument returns an independent copy of the canonical published
// document. The Problem extension is filled from the runtime catalog rather
// than being duplicated in the authored JSON.
func OpenAPIDocument() []byte {
	return bytes.Clone(publishedOpenAPI)
}

func mustPublishOpenAPI() []byte {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(openAPIBase, &document); err != nil {
		panic(fmt.Sprintf("invalid embedded control API OpenAPI document: %v", err))
	}
	catalog, err := json.Marshal(ProblemDescriptors())
	if err != nil {
		panic(fmt.Sprintf("encode control API Problem catalog: %v", err))
	}
	document["x-dorf-problems"] = catalog
	result, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("publish control API OpenAPI document: %v", err))
	}
	return append(result, '\n')
}
