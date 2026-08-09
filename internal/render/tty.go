// Package render turns a sieved report into something a human can read in one
// screenful.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/sieve"
	"github.com/imcitius/tgsieve/internal/textutil"
)

type Options struct {
	Color     bool
	Verbose   bool // show attributes for creates/deletes too
	ShowEmpty bool // list the unchanged units
	Explain   bool // list every hidden attribute and the rule that hid it
	MaxAttrs  int
	MaxUnits  int
	MaxValue  int
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
		for _, u := range rep.ErroredUnits {
			fmt.Fprintf(w, "  %s %s\n", p.red("✗"), p.bold(u.Path))
			for _, line := range textutil.CleanError(u.Error, 8) {
				fmt.Fprintf(w, "      %s\n", p.dim(line))
			}
		}
	}

	var danger, updates, creates, drift []sieve.Group
	for _, g := range rep.Groups {
		switch {
		case g.Drift:
			drift = append(drift, g)
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
	section(w, p, opts, "DRIFT (changed outside terraform)", drift, true)

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

	footer(w, p, rep, opts)
}

func section(w io.Writer, p painter, opts Options, title string, groups []sieve.Group, withAttrs bool) {
	if len(groups) == 0 {
		return
	}
	total := 0
	for _, g := range groups {
		total += g.Instances()
	}
	fmt.Fprintf(w, "\n%s %s\n", p.bold(title), p.dim(fmt.Sprintf("(%d)", total)))
	for _, g := range groups {
		renderGroup(w, p, opts, g, withAttrs)
	}
}

func renderGroup(w io.Writer, p painter, opts Options, g sieve.Group, withAttrs bool) {
	sym := p.action(g.Action, g.Action.Symbol())
	scope := g.Sample.Unit
	if len(g.Units) > 1 {
		scope = fmt.Sprintf("%d units", len(g.Units))
	}
	addr := g.Sample.BaseAddress
	if idx := g.IndexLabel(); idx != "" {
		addr += p.dim(idx)
	}
	count := ""
	if g.Instances() > 1 {
		count = p.dim(fmt.Sprintf(" ×%d", g.Instances()))
	}
	fmt.Fprintf(w, "  %s %s  %s%s\n", sym, p.bold(scope), addr, count)

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
		fmt.Fprintf(w, "      %s %s\n", p.dim("in"), p.dim(line))
	}

	// A destroy takes the whole resource with it; listing its attributes says
	// nothing a reader can act on.
	showAttrs := withAttrs && (g.Action != model.ActionDelete || opts.Verbose)
	if !showAttrs || len(g.Sample.Attrs) == 0 {
		if n := len(g.Sample.Attrs); n > 0 && !showAttrs {
			fmt.Fprintf(w, "      %s\n", p.dim(fmt.Sprintf("%d attributes (-v to show)", n)))
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
	for _, a := range attrs {
		val := ""
		if g.Varies[a.Path] {
			val = p.dim(varyLabel(g))
		} else {
			val = renderValue(p, a, opts.MaxValue)
		}
		mark := ""
		if a.ForcesReplace {
			mark = "  " + p.purple("forces replacement")
		}
		fmt.Fprintf(w, "      %-*s  %s%s\n", width, a.Path, val, mark)
	}
	if extra > 0 {
		fmt.Fprintf(w, "      %s\n", p.dim(fmt.Sprintf("… %d more attributes", extra)))
	}
	renderHidden(w, p, g)
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
	fmt.Fprintf(w, "      %s\n", p.dim(fmt.Sprintf("(%d attributes hidden by rules)", len(g.Sample.Hidden))))
}

func renderValue(p painter, a model.AttrChange, max int) string {
	after := ""
	switch {
	case a.Sensitive:
		after = p.dim("(sensitive)")
	case a.AfterUnknown:
		after = p.cyan("(known after apply)")
	default:
		after = p.green(fmtVal(a.After, max))
	}
	if a.Before == nil {
		return after
	}
	before := fmtVal(a.Before, max)
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
		parts = append(parts, p.cyan(fmt.Sprintf("!%d drift", k.Drift)))
	}
	if len(parts) == 0 {
		parts = append(parts, p.green("no changes"))
	}

	fmt.Fprintf(w, "\n%s  %s\n", p.bold("SUMMARY"), strings.Join(parts, "  "))
	fmt.Fprintf(w, "  %s\n", p.dim(fmt.Sprintf("%d units · %d with changes · %d unchanged · %d failed",
		rep.UnitsTotal, rep.UnitsChanged, len(rep.UnchangedUnits), len(rep.ErroredUnits))))

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
