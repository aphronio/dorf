package terminal

import (
	"strings"
	"unicode"
)

func attachmentBasename(filename string) string {
	var name strings.Builder
	for _, r := range filename {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			r = '_'
		}
		if name.Len()+len(string(r)) > 255 {
			break
		}
		name.WriteRune(r)
	}
	if name.Len() == 0 || name.String() == "." || name.String() == ".." {
		return "attachment"
	}
	return name.String()
}
