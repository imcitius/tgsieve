package render

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/imcitius/tgsieve/internal/sieve"
)

// GitHub writes GitHub Actions workflow commands. Failures become annotations
// against the file and line they came from, so they appear on the diff rather
// than only in the job log; the summary becomes a notice.
//
// https://docs.github.com/actions/reference/workflow-commands-for-github-actions
func GitHub(w io.Writer, rep *sieve.Report, opts Options) {
	for _, g := range rep.Failures {
		// The annotation is already attached to a line, so a message naming a
		// different one — the example the group happened to keep — would
		// contradict where it is shown.
		msg := escapeWorkflow(withoutLocation(g.Headline))
		locations := g.Locations
		if len(locations) == 0 {
			// No file to point at: still worth an annotation on the job.
			fmt.Fprintf(w, "::error title=%s::%s\n",
				escapeProperty(strings.Join(g.Units, ", ")), msg)
			continue
		}
		for _, loc := range locations {
			file, line := splitLocation(loc)
			fmt.Fprintf(w, "::error file=%s,line=%d,title=%s::%s\n",
				escapeProperty(file), line, escapeProperty(strings.Join(g.Units, ", ")), msg)
		}
	}

	k := rep.Kept
	if n := k.Delete + k.Replace; n > 0 {
		fmt.Fprintf(w, "::warning title=tgsieve::%s\n", escapeWorkflow(fmt.Sprintf(
			"%s will be destroyed or replaced (%d destroy, %d replace)", plural(n, "resource"), k.Delete, k.Replace)))
	}
	fmt.Fprintf(w, "::notice title=tgsieve::%s\n", escapeWorkflow(fmt.Sprintf(
		"%d create, %d update, %d destroy, %d replace across %s",
		k.Create, k.Update, k.Delete, k.Replace, plural(rep.UnitsChanged, "unit"))))
}

var atLocationRe = regexp.MustCompile(` at [^\s:]+:\d+`)

// withoutLocation removes the "at file:line" a diagnostic carries, for places
// that show the location themselves.
func withoutLocation(s string) string {
	return strings.TrimSpace(atLocationRe.ReplaceAllString(s, ""))
}

func splitLocation(loc string) (string, int) {
	i := strings.LastIndex(loc, ":")
	if i < 0 {
		return loc, 1
	}
	n, err := strconv.Atoi(loc[i+1:])
	if err != nil {
		return loc, 1
	}
	return loc[:i], n
}

// escapeWorkflow encodes the characters that would otherwise end the command
// early or inject another one.
func escapeWorkflow(s string) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	return r.Replace(s)
}

func escapeProperty(s string) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
	return r.Replace(s)
}
