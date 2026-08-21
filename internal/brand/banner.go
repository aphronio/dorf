package brand

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const Tagline = "Your agents. Your infrastructure. One API."

type terminalLogoLine struct {
	Glyphs string
	Colors []uint32
}

// SetupBanner renders the generated Dorf lockup. The generated data retains the
// source logo's per-cell colors while the wordmark uses the approved warm blend.
func SetupBanner(color bool) string {
	const (
		logoWidth     = 16
		wordmarkWidth = 34
		gap           = "   "
		wordmarkRGB   = uint32(0xD5C592)
		taglineRGB    = uint32(0x8F9D8D)
	)

	var output strings.Builder
	for row := range len(terminalLogo) {
		writeLogoLine(&output, terminalLogo[row], logoWidth, color)
		output.WriteString(gap)
		wordmarkRow := row - 1
		if wordmarkRow >= 0 && wordmarkRow < len(terminalWordmark) {
			writeColored(&output, terminalWordmark[wordmarkRow], wordmarkRGB, color)
		}
		output.WriteByte('\n')
	}

	totalWidth := logoWidth + len(gap) + wordmarkWidth
	padding := max(0, (totalWidth-utf8.RuneCountInString(Tagline))/2)
	output.WriteString(strings.Repeat(" ", padding))
	writeColored(&output, Tagline, taglineRGB, color)
	return output.String()
}

func writeLogoLine(output *strings.Builder, line terminalLogoLine, width int, color bool) {
	glyphs := []rune(line.Glyphs)
	for index, glyph := range glyphs {
		if color && index < len(line.Colors) && line.Colors[index] != 0 {
			writeColored(output, string(glyph), line.Colors[index], true)
			continue
		}
		output.WriteRune(glyph)
	}
	if missing := width - len(glyphs); missing > 0 {
		output.WriteString(strings.Repeat(" ", missing))
	}
}

func writeColored(output *strings.Builder, value string, rgb uint32, color bool) {
	if !color {
		output.WriteString(value)
		return
	}
	fmt.Fprintf(output, "\x1b[38;2;%d;%d;%dm%s\x1b[0m", rgb>>16, (rgb>>8)&0xff, rgb&0xff, value)
}
