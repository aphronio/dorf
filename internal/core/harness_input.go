package core

// HarnessInput contains materialized ordinary input. Durable attachment identity
// stays on Message; adapters receive verified image bytes rather than file paths.
type HarnessInput struct {
	DeveloperInstructions *string
	Text                  string
	Images                []HarnessImage
}

type HarnessImage struct {
	MediaType string
	Bytes     []byte
}
