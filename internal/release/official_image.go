package release

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed official_image.json
var officialImageBytes []byte

type officialImageDescriptor struct {
	ReleaseTag string `json:"release_tag"`
}

var officialImage = mustOfficialImageDescriptor()

// OfficialImageRelease returns the immutable Dorf release containing the
// Incus image selected by guided setup.
func OfficialImageRelease() string {
	return officialImage.ReleaseTag
}

func mustOfficialImageDescriptor() officialImageDescriptor {
	var descriptor officialImageDescriptor
	if err := json.Unmarshal(officialImageBytes, &descriptor); err != nil {
		panic(fmt.Sprintf("invalid official Incus image descriptor: %v", err))
	}
	if !regexpTag.MatchString(descriptor.ReleaseTag) {
		panic("invalid official Incus image descriptor")
	}
	return descriptor
}
