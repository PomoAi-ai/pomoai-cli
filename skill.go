package pomoai

import (
	_ "embed"
	"strings"
)

//go:embed skills/pomoai/SKILL.md
var skillMD string

// guideText is SKILL.md without its YAML frontmatter.
func guideText() string {
	s := strings.ReplaceAll(skillMD, "\r\n", "\n")
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		if _, body, ok := strings.Cut(rest, "\n---\n"); ok {
			s = body
		}
	}
	return strings.TrimSpace(s)
}
