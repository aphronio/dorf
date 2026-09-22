package core

// HarnessInput contains materialized ordinary input. Durable attachment identity
// is supplied by the caller; adapters receive image and audio bytes rather than file paths.
type HarnessInput struct {
	Observation           bool
	DeveloperInstructions *string
	Text                  string
	Images                []HarnessImage
	Audio                 []HarnessAudio
}

type HarnessImage struct {
	MediaType string
	Bytes     []byte
}

// InputCapabilities describes native audio usable with this Session's exact model.
// Empty media types mean audio is unsupported or the model metadata is unknown.
type InputCapabilities struct {
	Model           string   `json:"model"`
	AudioMediaTypes []string `json:"audio_media_types"`
	MaxAudioBytes   int      `json:"max_audio_bytes"`
}

type HarnessAudio struct {
	MediaType string
	Bytes     []byte
}
