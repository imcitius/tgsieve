// Package textutil cleans up the terminal art terraform wraps its errors in.
package textutil

import (
	"regexp"
	"strings"
)

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	boxRe  = regexp.MustCompile(`^[\s│╷╵─┌┐└┘├┤╭╮╰╯|]*`)
	// The working directory terragrunt reports is inside its cache, two hashed
	// path segments deep. Nobody needs to read those.
	cacheRe  = regexp.MustCompile(`/\.terragrunt-cache/[^\s/]+(/[^\s/]+)*`)
	headerRe = regexp.MustCompile(`^\d+ errors? occurred:`)

	// Fragments that make two reports of the same problem look different.
	quotedRe = regexp.MustCompile(`"[^"]*"|'[^']*'|` + "`[^`]*`")
	// Providers name the offending object in parentheses:
	// `creating S3 Bucket (logs-prod-eu): AccessDenied`.
	parenRe = regexp.MustCompile(`\([^)]*\)`)
	addrRe  = regexp.MustCompile(`\b(?:module\.[\w-]+\.)*[a-z][\w-]*\.[\w-]+(?:\[[^\]]*\])?\b`)
	hexRe   = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	numRe   = regexp.MustCompile(`\b\d+\b`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// NormalizeError reduces an error headline to its shape, so the same failure
// reported against different resources, regions or line numbers groups as one
// cause. Only used as a grouping key — what gets printed is always a real
// message, never this.
func NormalizeError(s string) string {
	out := StripANSI(s)
	out = quotedRe.ReplaceAllString(out, `"…"`)
	out = parenRe.ReplaceAllString(out, "(…)")
	out = addrRe.ReplaceAllString(out, "…")
	out = hexRe.ReplaceAllString(out, "…")
	out = numRe.ReplaceAllString(out, "N")
	return strings.TrimSpace(spaceRe.ReplaceAllString(out, " "))
}

// StripANSI removes color escape sequences.
func StripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// SplitErrors breaks terragrunt's aggregated error blob ("3 errors occurred:"
// followed by "* ..." entries) into one string per underlying failure, so a
// unit can be given its own error instead of the whole stack's.
func SplitErrors(s string) []string {
	clean := StripANSI(s)
	var out, cur []string
	flush := func() {
		if joined := strings.TrimSpace(strings.Join(cur, "\n")); joined != "" {
			out = append(out, joined)
		}
		cur = nil
	}
	for _, l := range strings.Split(clean, "\n") {
		trimmed := strings.TrimSpace(l)
		switch {
		case headerRe.MatchString(trimmed):
			flush()
		case strings.HasPrefix(trimmed, "* "):
			flush()
			cur = append(cur, trimmed[2:])
		default:
			cur = append(cur, l)
		}
	}
	flush()
	if len(out) == 0 {
		return []string{strings.TrimSpace(clean)}
	}
	return out
}

// CleanError turns a terraform/terragrunt error blob into the lines a human
// actually needs: no escape codes, no box drawing, no blank runs. If the blob
// contains a real "Error:" line, everything before it is dropped as framing.
func CleanError(s string, max int) []string {
	raw := strings.Split(cacheRe.ReplaceAllString(StripANSI(s), ""), "\n")
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

var locationRe = regexp.MustCompile(`\bat ([^\s:]+:\d+)`)

// Location pulls "file:line" out of a diagnostic, so failures that share a
// message but differ in where they happen can be listed rather than collapsed
// into one another.
func Location(s string) string {
	if m := locationRe.FindStringSubmatch(StripANSI(s)); m != nil {
		return m[1]
	}
	// terraform's text output words it differently.
	if m := regexp.MustCompile(`on (\S+) line (\d+)`).FindStringSubmatch(StripANSI(s)); m != nil {
		return m[1] + ":" + m[2]
	}
	return ""
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
