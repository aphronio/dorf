package core

// HarnessInput contains materialized ordinary input. Durable attachment identity
// is supplied by the caller; adapters receive verified image bytes rather than file paths.
type HarnessInput struct {
	Observation           bool
	DeveloperInstructions *string
	Text                  string
	Images                []HarnessImage
}

type HarnessImage struct {
	MediaType string
	Bytes     []byte
}
