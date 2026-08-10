// Package render turns a sieved report into something a human can read in one
// screenful.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/sieve"
)

type Options struct {
	Color      bool
	Timings    bool // list the slowest units
	Verbose    bool // show attributes for creates/deletes too
	ShowEmpty  bool // list the unchanged units
	Explain    bool // list every hidden attribute and the rule that hid it
	MaxAttrs   int
	MaxUnits   int
	MaxValue   int
	MaxTimings int
	// MaxBytes caps markdown output so a CI comment is trimmed on purpose
	// rather than rejected for being too long.
	MaxBytes int
}

func (o Options) withDefaults() Options {
	if o.MaxAttrs == 0 {
		o.MaxAttrs = 12
	}
	if o.MaxUnits == 0 {
		o.MaxUnits = 6
	}
	if o.MaxValue == 0 {
		o.MaxValue = 72
	}
	if o.MaxTimings == 0 {
		o.MaxTimings = 10
	}
	return o
}

type painter struct{ on bool }

func (p painter) c(code, s string) string {
	if !p.on {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}
func (p painter) red(s string) string    { return p.c("31", s) }
func (p painter) green(s string) string  { return p.c("32", s) }
func (p painter) yellow(s string) string { return p.c("33", s) }
func (p painter) blue(s string) string   { return p.c("34", s) }
func (p painter) purple(s string) string { return p.c("35", s) }
func (p painter) cyan(s string) string   { return p.c("36", s) }
func (p painter) bold(s string) string   { return p.c("1", s) }
func (p painter) dim(s string) string    { return p.c("2", s) }

func (p painter) action(a model.Action, s string) string {
	switch a {
	case model.ActionCreate:
		return p.green(s)
	case model.ActionUpdate:
		return p.yellow(s)
	case model.ActionDelete:
		return p.red(s)
	case model.ActionReplace:
		return p.purple(s)
	default:
		return p.dim(s)
	}
}

// TTY writes the human report.
func TTY(w io.Writer, rep *sieve.Report, opts Options) {
	opts = opts.withDefaults()
	p := painter{on: opts.Color}

	if len(rep.ErroredUnits) > 0 {
		fmt.Fprintf(w, "\n%s\n", p.bold(p.red(fmt.Sprintf("FAILED (%d)", len(rep.ErroredUnits)))))
		// Naming the binary only helps when the failure is the kind a wrong
		// binary causes; otherwise it is one more line between the reader and
		// the error.
		if rep.TFPath != "" && binaryMightBeTheProblem(rep) {
			who := "terragrunt ran " + rep.TFPath
			if rep.Direct {
				who = "ran " + rep.TFPath
			}
			fmt.Fprintf(w, "  %s\n", p.dim(who+" — use --tf-path or TG_TF_PATH to choose another binary"))
		}
		for _, g := range rep.Failures {
			scope := g.Units[0]
			if len(g.Units) > 1 {
				scope = plural(len(g.Units), "unit") + ", same error"
			}
			note := ""
			if g.Count > len(g.Units) {
				// The same failure once per affected resource: the count is
				// the news, the repetition is not.
				note = p.dim(fmt.Sprintf("  ×%d", g.Count))
			}
			// Wording differences are only worth mentioning when they are not
			// simply one per occurrence: "20 wordings" for 20 resources named
			// in 20 messages tells the reader nothing.
			if g.Variants > 1 && g.Variants < g.Count {
				note += p.dim(fmt.Sprintf("  (%d wordings, showing one)", g.Variants))
			}
			fmt.Fprintf(w, "  %s %s%s\n", p.red("✗"), p.bold(scope), note)
			if len(g.Units) > 1 {
				shown := g.Units
				extra := 0
				if len(shown) > opts.MaxUnits {
					extra = len(shown) - opts.MaxUnits
					shown = shown[:opts.MaxUnits]
				}
				line := strings.Join(shown, ", ")
				if extra > 0 {
					line += fmt.Sprintf(", +%d more", extra)
				}
				fmt.Fprintf(w, "      %s\n", p.dim(line))
			}
			for _, line := range g.Detail {
				fmt.Fprintf(w, "      %s\n", p.dim(line))
			}
			// The same message from several places is several things to fix.
			if rest := others(g.Locations, g.Detail); len(rest) > 0 {
				shown, extra := capList(rest, opts.MaxUnits)
				line := "also at " + strings.Join(shown, ", ")
				if extra > 0 {
					line += fmt.Sprintf(", +%d more", extra)
				}
				fmt.Fprintf(w, "      %s\n", p.dim(line))
			}
		}
	}

	if len(rep.SkippedUnits) > 0 {
		fmt.Fprintf(w, "\n%s\n", p.bold(fmt.Sprintf("NOT RUN (%d)", len(rep.SkippedUnits))))
		for _, u := range rep.SkippedUnits {
			fmt.Fprintf(w, "  %s %s  %s\n", p.dim("·"), u.Path, p.dim(u.Reason))
		}
	}

	var danger, updates, creates, drift, driftLeft []sieve.Group
	for _, g := range rep.Groups {
		switch {
		case g.Drift && g.Sample.DriftReverted:
			drift = append(drift, g)
		case g.Drift:
			driftLeft = append(driftLeft, g)
		case g.Action == model.ActionReplace, g.Action == model.ActionDelete:
			danger = append(danger, g)
		case g.Action == model.ActionUpdate:
			updates = append(updates, g)
		default:
			creates = append(creates, g)
		}
	}

	section(w, p, opts, "DESTROY / REPLACE", danger, true)
	section(w, p, opts, "UPDATE", updates, true)
	section(w, p, opts, "CREATE", creates, opts.Verbose)
	section(w, p, opts, "DRIFT — this plan puts it back", drift, true)
	section(w, p, opts, "DRIFT — this plan leaves it", driftLeft, true)

	if len(rep.Outputs) > 0 {
		n := 0
		for _, o := range rep.Outputs {
			n += len(o)
		}
		fmt.Fprintf(w, "\n%s\n", p.bold("OUTPUTS"))
		units := make([]string, 0, len(rep.Outputs))
		for u := range rep.Outputs {
			units = append(units, u)
		}
		sort.Strings(units)
		for _, u := range units {
			paths := make([]string, 0, len(rep.Outputs[u]))
			for _, o := range rep.Outputs[u] {
				paths = append(paths, o.Path)
			}
			fmt.Fprintf(w, "  %s %s\n", p.dim(u), strings.Join(paths, ", "))
		}
	}

	timings(w, p, rep, opts)
	footer(w, p, rep, opts)
}

func timings(w io.Writer, p painter, rep *sieve.Report, opts Options) {
	if !opts.Timings || len(rep.Timings) == 0 {
		return
	}
	shown := rep.Timings
	if len(shown) > opts.MaxTimings {
		shown = shown[:opts.MaxTimings]
	}
	fmt.Fprintf(w, "\n%s %s\n", p.bold("SLOWEST UNITS"), p.dim(fmt.Sprintf("(of %d)", len(rep.Timings))))
	width := 0
	for _, t := range shown {
		if len(t.Path) > width {
			width = len(t.Path)
		}
	}
	for _, t := range shown {
		note := p.dim("no changes")
		if t.Changes > 0 {
			note = p.dim(plural(t.Changes, "change"))
		}
		if t.Reused {
			label := "  (reused plan"
			if t.Age > time.Hour {
				label += ", measured " + humanAge(t.Age) + " ago"
			}
			note += p.dim(label + ")")
		}
		fmt.Fprintf(w, "  %-*s  %8s  %s\n", width, t.Path, t.Duration.Round(time.Millisecond), note)
	}
}

// section prints one severity band, nested three deep: the scope a change
// belongs to, then the resource, then the attributes. Repeating the directory
// on every resource line — which is what a flat list does — buries the one
// piece of context a reader navigates by.
func section(w io.Writer, p painter, opts Options, title string, groups []sieve.Group, withAttrs bool) {
	if len(groups) == 0 {
		return
	}
	total := 0
	for _, g := range groups {
		total += g.Instances()
	}
	fmt.Fprintf(w, "\n%s %s\n", p.bold(title), p.dim(fmt.Sprintf("(%d)", total)))
	for _, sc := range scopes(groups) {
		renderScope(w, p, opts, sc, withAttrs)
	}
}

// scope is a set of changes that share the same place: one unit, or the same
// several units when a change was collapsed across them.
type scope struct {
	units  []string
	groups []sieve.Group
}

func (s scope) multi() bool { return len(s.units) > 1 }

// scopes buckets groups by where they happened, keeping changes that span the
// same set of units together.
func scopes(groups []sieve.Group) []scope {
	index := map[string]int{}
	var out []scope
	for _, g := range groups {
		key := strings.Join(g.Units, "\x00")
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, scope{units: g.Units})
		}
		out[i].groups = append(out[i].groups, g)
	}
	// Widest blast radius first, then alphabetically: a change touching twelve
	// units is a different kind of news from one touching a single directory.
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := len(out[i].units), len(out[j].units); a != b {
			return a > b
		}
		return strings.Join(out[i].units, ",") < strings.Join(out[j].units, ",")
	})
	return out
}

func renderScope(w io.Writer, p painter, opts Options, sc scope, withAttrs bool) {
	label := ""
	switch {
	case len(sc.units) == 0:
		label = p.dim("(unknown unit)")
	case sc.multi():
		shown := sc.units
		extra := 0
		if len(shown) > opts.MaxUnits {
			extra = len(shown) - opts.MaxUnits
			shown = shown[:opts.MaxUnits]
		}
		list := strings.Join(shown, ", ")
		if extra > 0 {
			list += fmt.Sprintf(", +%d more", extra)
		}
		label = p.bold(plural(len(sc.units), "unit")) + "  " + p.dim(list)
	default:
		label = p.bold(sc.units[0])
	}
	fmt.Fprintf(w, "  %s\n", label)
	for _, g := range sc.groups {
		renderGroup(w, p, opts, g, withAttrs)
	}
}

func renderGroup(w io.Writer, p painter, opts Options, g sieve.Group, withAttrs bool) {
	sym := p.action(g.Action, g.Action.Symbol())
	addr := g.Sample.BaseAddress
	if idx := g.IndexLabel(); idx != "" {
		addr += p.dim(idx)
	}
	count := ""
	if g.Instances() > 1 {
		count = p.dim(fmt.Sprintf(" ×%d", g.Instances()))
	}
	fmt.Fprintf(w, "    %s %s%s\n", sym, addr, count)

	// A destroy takes the whole resource with it; listing its attributes says
	// nothing a reader can act on.
	showAttrs := withAttrs && (g.Action != model.ActionDelete || opts.Verbose)
	if !showAttrs || len(g.Sample.Attrs) == 0 {
		if n := len(g.Sample.Attrs); n > 0 && !showAttrs {
			fmt.Fprintf(w, "        %s\n", p.dim(plural(n, "attribute")+" (-v to show)"))
		}
		renderHidden(w, p, g)
		return
	}

	attrs := g.Sample.Attrs
	extra := 0
	if len(attrs) > opts.MaxAttrs {
		extra = len(attrs) - opts.MaxAttrs
		attrs = attrs[:opts.MaxAttrs]
	}
	width := 0
	for _, a := range attrs {
		if len(a.Path) > width {
			width = len(a.Path)
		}
	}
	if width > 40 {
		width = 40
	}
	for i, a := range attrs {
		val := ""
		if i < len(g.Varies) && g.Varies[i] {
			val = p.dim(varyLabel(g))
		} else {
			val = renderValue(p, a, opts.MaxValue)
		}
		mark := ""
		if a.ForcesReplace {
			mark = "  " + p.purple("forces replacement")
		}
		fmt.Fprintf(w, "        %-*s  %s%s\n", width, a.Path, val, mark)
	}
	if extra > 0 {
		fmt.Fprintf(w, "        %s\n", p.dim("… "+plural(extra, "more attribute")))
	}
	renderHidden(w, p, g)
}

func humanAge(d time.Duration) string {
	if d > 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// others drops the location already visible in the shown message, so the list
// says what the reader cannot already see.
func others(locations []string, detail []string) []string {
	shown := strings.Join(detail, "\n")
	out := make([]string, 0, len(locations))
	for _, loc := range locations {
		if strings.Contains(shown, loc) {
			continue
		}
		out = append(out, loc)
	}
	return out
}

// capList truncates a list, returning what to show and how many were left out.
func capList(items []string, max int) ([]string, int) {
	if max <= 0 || len(items) <= max {
		return items, 0
	}
	return items[:max], len(items) - max
}

// binaryMightBeTheProblem spots the failures a mismatched terraform/tofu
// produces: initialization, lock files and provider resolution.
func binaryMightBeTheProblem(rep *sieve.Report) bool {
	for _, g := range rep.Failures {
		h := strings.ToLower(g.Headline)
		for _, hint := range []string{"initialization required", "lock file", "backend", "provider requirements", "plugin"} {
			if strings.Contains(h, hint) {
				return true
			}
		}
	}
	return false
}

func varyLabel(g sieve.Group) string {
	if len(g.Units) > 1 {
		return "(varies by unit)"
	}
	return "(varies by instance)"
}

func renderHidden(w io.Writer, p painter, g sieve.Group) {
	if len(g.Sample.Hidden) == 0 {
		return
	}
	fmt.Fprintf(w, "        %s\n", p.dim("("+plural(len(g.Sample.Hidden), "attribute")+" hidden by rules)"))
}

func renderValue(p painter, a model.AttrChange, max int) string {
	switch a.Kind {
	case model.KindReordered:
		return p.dim(fmt.Sprintf("reordered (%s, same members)", plural(a.Count, "item")))
	case model.KindAdded:
		return p.green("+ " + fmtVal(a.After, max))
	case model.KindRemoved:
		return p.red("- " + fmtVal(a.Before, max))
	}
	after := ""
	switch {
	case a.Sensitive:
		after = p.dim("(sensitive)")
	case a.AfterUnknown:
		after = p.cyan("(known after apply)")
	default:
		after = p.green(fmtVal(a.After, max))
	}
	before := fmtVal(a.Before, max)
	if a.Before == nil {
		// An attribute that had no prior value at all: saying only what it
		// became reads as a statement of fact rather than as a change.
		before = "(unset)"
	}
	if a.Sensitive {
		before = "(sensitive)"
	}
	return p.red(before) + p.dim(" → ") + after
}

func fmtVal(v any, max int) string {
	var s string
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		s = strconv_Quote(t)
	case float64:
		if t == float64(int64(t)) {
			s = fmt.Sprintf("%d", int64(t))
		} else {
			s = fmt.Sprintf("%g", t)
		}
	case bool:
		s = fmt.Sprintf("%t", t)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			s = fmt.Sprint(v)
		} else {
			s = string(b)
		}
	}
	s = strings.ReplaceAll(s, "\\n", "↵")
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}

func strconv_Quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(b)
}

func footer(w io.Writer, p painter, rep *sieve.Report, opts Options) {
	k := rep.Kept
	parts := []string{}
	if k.Replace > 0 {
		parts = append(parts, p.purple(fmt.Sprintf("±%d replace", k.Replace)))
	}
	if k.Delete > 0 {
		parts = append(parts, p.red(fmt.Sprintf("-%d destroy", k.Delete)))
	}
	if k.Update > 0 {
		parts = append(parts, p.yellow(fmt.Sprintf("~%d update", k.Update)))
	}
	if k.Create > 0 {
		parts = append(parts, p.green(fmt.Sprintf("+%d create", k.Create)))
	}
	if k.Drift > 0 {
		label := fmt.Sprintf("!%d drift", k.Drift)
		if k.DriftLeft > 0 {
			label += fmt.Sprintf(" (%d not addressed)", k.DriftLeft)
		}
		parts = append(parts, p.cyan(label))
	}
	if len(parts) == 0 {
		parts = append(parts, p.green("no changes"))
	}

	fmt.Fprintf(w, "\n%s  %s\n", p.bold("SUMMARY"), strings.Join(parts, "  "))
	line := fmt.Sprintf("%s · %d with changes · %d unchanged · %d failed",
		plural(rep.UnitsTotal, "unit"), rep.UnitsChanged, len(rep.UnchangedUnits), len(rep.ErroredUnits))
	if n := len(rep.SkippedUnits); n > 0 {
		line += fmt.Sprintf(" · %d not run", n)
	}
	if rep.Wall > 0 {
		line += " · " + rep.Wall.Round(100*time.Millisecond).String()
	}
	fmt.Fprintf(w, "  %s\n", p.dim(line))
	if rep.Severity != "" {
		counts := []string{}
		for _, level := range []string{"high", "medium", "low"} {
			if n := rep.SeverityCounts[level]; n > 0 {
				counts = append(counts, fmt.Sprintf("%d %s", n, level))
			}
		}
		fmt.Fprintf(w, "  %s\n", p.dim("severity: "+strings.Join(counts, ", ")))
	}
	if rep.NoRefresh {
		fmt.Fprintf(w, "  %s\n", p.yellow("state was not refreshed: anything changed outside terraform is invisible here"))
	}
	if len(rep.Timings) > 0 && !opts.Timings {
		slowest := rep.Timings[0]
		fmt.Fprintf(w, "  %s\n", p.dim(fmt.Sprintf("slowest: %s (%s) — --timings for the rest",
			slowest.Path, slowest.Duration.Round(time.Millisecond))))
	}

	if n := len(rep.ExpiredRules); n > 0 {
		fmt.Fprintf(w, "  %s\n", p.yellow(fmt.Sprintf("%s expired and no longer hide anything: %s",
			plural(n, "rule"), strings.Join(rep.ExpiredRules, "; "))))
	}
	if rep.Normalized > 0 {
		fmt.Fprintf(w, "  %s\n", p.dim(fmt.Sprintf("normalized: %s treated as no change (normalize rules)",
			plural(rep.Normalized, "difference"))))
	}
	if rep.HiddenAttrs > 0 || rep.HiddenResources > 0 {
		fmt.Fprintf(w, "  %s\n", p.dim(fmt.Sprintf("sieved: %d attributes and %d resources hidden by %s%s",
			rep.HiddenAttrs, rep.HiddenResources, plural(len(rep.RuleStats), "rule"), hint(opts.Explain, " (--explain)"))))
		if opts.Explain {
			for _, s := range rep.RuleStats {
				fmt.Fprintf(w, "      %s %s\n", p.dim(fmt.Sprintf("%4d attrs, %3d resources", s.Attrs, s.Res)), s.Rule)
			}
			shown := rep.Explanations
			if len(shown) > 200 {
				shown = shown[:200]
			}
			for _, e := range shown {
				fmt.Fprintf(w, "      %s %s %s\n", p.dim(e.Unit), e.Address+"."+e.Path, p.dim("← "+e.Rule))
			}
			if len(rep.Explanations) > len(shown) {
				fmt.Fprintf(w, "      %s\n", p.dim(fmt.Sprintf("… %d more", len(rep.Explanations)-len(shown))))
			}
		}
	}

	if len(rep.UnchangedUnits) > 0 {
		if opts.ShowEmpty {
			fmt.Fprintf(w, "  %s\n", p.dim("unchanged: "+strings.Join(rep.UnchangedUnits, ", ")))
		} else {
			fmt.Fprintf(w, "  %s\n", p.dim(fmt.Sprintf("unchanged: %d units (--show-empty)", len(rep.UnchangedUnits))))
		}
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func hint(on bool, s string) string {
	if on {
		return ""
	}
	return s
}
