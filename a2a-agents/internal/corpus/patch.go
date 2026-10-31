package corpus

import (
	"regexp"
	"strings"
)

var codeBlock = regexp.MustCompile("(?s)```(?:python|py)?\\s*\\n(.*?)```")

// ExtractFunction pulls the proposed function out of a model's answer: the
// first fenced code block, or the whole answer if there is none.
func ExtractFunction(answer string) string {
	if m := codeBlock.FindStringSubmatch(answer); m != nil {
		return strings.TrimSpace(m[1]) + "\n"
	}
	return strings.TrimSpace(answer) + "\n"
}

// Apply replaces the issue's function in its module with fix: everything
// from "def <name>(" up to the next top-level statement.
func Apply(module, function, fix string) string {
	lines := strings.Split(module, "\n")
	start, end := -1, len(lines)
	for i, l := range lines {
		if start < 0 && strings.HasPrefix(l, "def "+function+"(") {
			start = i
			continue
		}
		if start >= 0 && l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			end = i
			break
		}
	}
	if start < 0 {
		return strings.TrimRight(module, "\n") + "\n\n" + fix
	}
	// Blank lines before the next statement separate it; keep them.
	for end-1 > start && lines[end-1] == "" {
		end--
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, strings.TrimRight(fix, "\n"))
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}
