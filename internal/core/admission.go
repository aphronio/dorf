package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxClientReferenceLength    = 255
	MaxInputBytes               = 1 << 20
	MaxAttachments              = 4
	MaxAttachmentFilenameLength = 255
	// MaxImagePixels bounds decoder memory before Dorf fully decodes an
	// accepted raster image. Compressed byte limits alone do not bound that use.
	MaxImagePixels = 25_000_000
)

func ValidClientReference(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= MaxClientReferenceLength && !strings.ContainsRune(value, 0)
}

func ValidAttachmentFilename(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "/\\") && utf8.RuneCountInString(value) <= MaxAttachmentFilenameLength && strings.IndexFunc(value, unicode.IsControl) < 0
}

// SessionAdmission is the complete admitted execution configuration.
type SessionAdmission struct {
	KeepRunning        bool
	CreatedByClientID  string
	ClientReference    string
	AdmissionKey       string
	AgentsMD           string
	SandboxProfile     string
	ProviderConnection string
	Model              string
	ReasoningEffort    string
}

// ValidDeveloperInstructions validates an optional complete application instruction snapshot.
func ValidDeveloperInstructions(value *string) bool {
	return value == nil || (utf8.ValidString(*value) && !strings.ContainsRune(*value, 0) && len(*value) <= MaxInputBytes)
}

func SameDeveloperInstructions(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
