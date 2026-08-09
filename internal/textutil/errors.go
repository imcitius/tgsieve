// Package textutil cleans up the terminal art terraform wraps its errors in.
package textutil

import (
	"regexp"
	"strings"
)

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	boxRe  = regexp.MustCompile(`^[\s│╷╵─┌┐└┘├┤╭╮╰╯|]*`)
)

// StripANSI removes color escape sequences.
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// CleanError turns a terraform/terragrunt error blob into the lines a human
// actually needs: no escape codes, no box drawing, no blank runs. If the blob
// contains a real "Error:" line, everything before it is dropped as framing.
func CleanError(s string, max int) []string {
	raw := strings.Split(StripANSI(s), "\n")
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimRight(boxRe.ReplaceAllString(l, ""), " \t")
		if l == "" || strings.Trim(l, "─│╷╵ ") == "" {
			continue
		}
		if n := len(lines); n > 0 && lines[n-1] == l {
			continue
		}
		lines = append(lines, l)
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "Error:") {
			lines = lines[i:]
			break
		}
	}
	if max > 0 && len(lines) > max {
		lines = append(lines[:max:max], "…")
	}
	return lines
}

// Headline is the single most informative line of an error blob, for the live
// progress feed where a 30-line dump would bury everything else.
func Headline(s string) string {
	lines := CleanError(s, 0)
	for _, l := range lines {
		if strings.HasPrefix(l, "Error:") {
			return l
		}
	}
	if len(lines) > 0 {
		return lines[0]
	}
	return strings.TrimSpace(StripANSI(s))
}
