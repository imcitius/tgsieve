package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/sieve"
)

// DefaultMaxBytes keeps a comment under GitHub's 65536-character limit with
// room for whatever the CI tool wraps around it. A report that is silently
// rejected for being too long is worse than one that says it was trimmed.
const DefaultMaxBytes = 55000

// Markdown renders the report for a pull request comment: dangerous changes
// open, everything else folded away, and the whole thing bounded in size.
func Markdown(w io.Writer, rep *sieve.Report, opts Options) {
	opts = opts.withDefaults()
	limit := opts.MaxBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	b := &budget{limit: limit}

	b.line(headline(rep))
	b.line("")
	b.line(counts(rep))

	if n := rep.Kept.Delete + rep.Kept.Replace; n > 0 {
		b.line("")
		b.line(fmt.Sprintf("> **%s will be destroyed or replaced.**", plural(n, "resource")))
	}
	if rep.NoRefresh {
		b.line("")
		b.line("> State was not refreshed: anything changed outside terraform is invisible here.")
	}

	if len(rep.Failures) > 0 {
		b.line("")
		b.line(fmt.Sprintf("### Failed (%d)", len(rep.ErroredUnits)))
		for _, g := range rep.Failures {
			b.line("")
			b.line(fmt.Sprintf("**%s**", strings.Join(g.Units, ", ")))
			b.line("")
			b.line("```")
			for _, line := range g.Detail {
				b.line(line)
			}
			b.line("```")
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

	// Destructive changes are never folded: the reader must not have to click
	// to find out something is being destroyed.
	mdSection(b, opts, "Destroy / replace", danger, false, true)
	mdSection(b, opts, "Drift this plan leaves", driftLeft, false, true)
	mdSection(b, opts, "Update", updates, true, true)
	mdSection(b, opts, "Create", creates, true, false)
	mdSection(b, opts, "Drift this plan puts back", drift, true, true)

	b.line("")
	b.line(mdFooter(rep))

	if b.trimmed {
		b.forceLine("")
		b.forceLine(fmt.Sprintf("_Output trimmed at %d characters. Run `tgsieve plan` locally, or see the job log, for the rest._", limit))
	}
	fmt.Fprint(w, b.String())
}

func headline(rep *sieve.Report) string {
	switch {
	case len(rep.ErroredUnits) > 0:
		return "## tgsieve — failed"
	case !rep.HasChanges():
		return "## tgsieve — no changes"
	default:
		return "## tgsieve"
	}
}

func counts(rep *sieve.Report) string {
	k := rep.Kept
	parts := []string{}
	if k.Replace > 0 {
		parts = append(parts, fmt.Sprintf("**±%d replace**", k.Replace))
	}
	if k.Delete > 0 {
		parts = append(parts, fmt.Sprintf("**-%d destroy**", k.Delete))
	}
	if k.Update > 0 {
		parts = append(parts, fmt.Sprintf("~%d update", k.Update))
	}
	if k.Create > 0 {
		parts = append(parts, fmt.Sprintf("+%d create", k.Create))
	}
	if k.Drift > 0 {
		label := fmt.Sprintf("!%d drift", k.Drift)
		if k.DriftLeft > 0 {
			label += fmt.Sprintf(" (%d not addressed)", k.DriftLeft)
		}
		parts = append(parts, label)
	}
	if len(parts) == 0 {
		parts = append(parts, "no changes")
	}
	tail := fmt.Sprintf("%s · %d with changes · %d unchanged · %d failed",
		plural(rep.UnitsTotal, "unit"), rep.UnitsChanged, len(rep.UnchangedUnits), len(rep.ErroredUnits))
	if rep.Wall > 0 {
		tail += " · " + rep.Wall.Round(100000000).String()
	}
	return strings.Join(parts, " · ") + "\n\n" + tail
}

func mdSection(b *budget, opts Options, title string, groups []sieve.Group, fold, withAttrs bool) {
	if len(groups) == 0 {
		return
	}
	total := 0
	for _, g := range groups {
		total += g.Instances()
	}
	b.line("")
	if fold {
		b.line(fmt.Sprintf("<details><summary><b>%s (%d)</b></summary>", title, total))
		b.line("")
	} else {
		b.line(fmt.Sprintf("### %s (%d)", title, total))
	}

	for _, sc := range scopes(groups) {
		b.line("")
		if sc.multi() {
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
			b.line(fmt.Sprintf("**%s** — %s", plural(len(sc.units), "unit"), list))
		} else if len(sc.units) > 0 {
			b.line(fmt.Sprintf("**%s**", sc.units[0]))
		}
		for _, g := range scopeGroups(sc) {
			mdGroup(b, opts, g, withAttrs)
		}
	}

	if fold {
		b.line("")
		b.line("</details>")
	}
}

// scopeGroups lets the markdown renderer walk the same scope structure the
// terminal renderer builds.
func scopeGroups(sc scope) []sieve.Group { return sc.groups }

func mdGroup(b *budget, opts Options, g sieve.Group, withAttrs bool) {
	addr := g.Sample.BaseAddress + g.IndexLabel()
	count := ""
	if g.Instances() > 1 {
		count = fmt.Sprintf(" ×%d", g.Instances())
	}
	b.line(fmt.Sprintf("- `%s` `%s`%s", g.Action.Symbol(), addr, count))

	showAttrs := withAttrs && (g.Action != model.ActionDelete || opts.Verbose)
	if !showAttrs || len(g.Sample.Attrs) == 0 {
		return
	}
	attrs := g.Sample.Attrs
	extra := 0
	if len(attrs) > opts.MaxAttrs {
		extra = len(attrs) - opts.MaxAttrs
		attrs = attrs[:opts.MaxAttrs]
	}
	for i, a := range attrs {
		value := ""
		if i < len(g.Varies) && g.Varies[i] {
			value = "_" + varyLabel(g) + "_"
		} else {
			value = mdValue(a, opts.MaxValue)
		}
		suffix := ""
		if a.ForcesReplace {
			suffix = " — **forces replacement**"
		}
		b.line(fmt.Sprintf("  - `%s` %s%s", a.Path, value, suffix))
	}
	if extra > 0 {
		b.line(fmt.Sprintf("  - _… %s_", plural(extra, "more attribute")))
	}
	if n := len(g.Sample.Hidden); n > 0 {
		b.line(fmt.Sprintf("  - _(%s hidden by rules)_", plural(n, "attribute")))
	}
}

func mdValue(a model.AttrChange, max int) string {
	switch a.Kind {
	case model.KindReordered:
		return fmt.Sprintf("_reordered (%s, same members)_", plural(a.Count, "item"))
	case model.KindAdded:
		return "added `" + mdCode(fmtVal(a.After, max)) + "`"
	case model.KindRemoved:
		return "removed `" + mdCode(fmtVal(a.Before, max)) + "`"
	}
	after := "`" + mdCode(fmtVal(a.After, max)) + "`"
	switch {
	case a.Sensitive:
		after = "_(sensitive)_"
	case a.AfterUnknown:
		after = "_(known after apply)_"
	}
	if a.Before == nil {
		return after
	}
	before := "`" + mdCode(fmtVal(a.Before, max)) + "`"
	if a.Sensitive {
		before = "_(sensitive)_"
	}
	return before + " → " + after
}

// mdCode keeps a value from breaking out of its inline code span.
func mdCode(s string) string { return strings.ReplaceAll(s, "`", "'") }

func mdFooter(rep *sieve.Report) string {
	notes := []string{}
	if rep.HiddenAttrs > 0 || rep.HiddenResources > 0 {
		notes = append(notes, fmt.Sprintf("sieved %s and %s with %s",
			plural(rep.HiddenAttrs, "attribute"), plural(rep.HiddenResources, "resource"),
			plural(len(rep.RuleStats), "rule")))
	}
	if rep.Normalized > 0 {
		notes = append(notes, fmt.Sprintf("normalized %s", plural(rep.Normalized, "difference")))
	}
	if n := len(rep.ExpiredRules); n > 0 {
		notes = append(notes, fmt.Sprintf("**%s expired and no longer hide anything**", plural(n, "rule")))
	}
	if len(rep.SkippedUnits) > 0 {
		notes = append(notes, fmt.Sprintf("%s not run", plural(len(rep.SkippedUnits), "unit")))
	}
	if rep.TFPath != "" && len(rep.ErroredUnits) > 0 {
		if rep.Direct {
			notes = append(notes, "ran "+rep.TFPath)
		} else {
			notes = append(notes, "terragrunt ran "+rep.TFPath)
		}
	}
	if len(notes) == 0 {
		return "<sub>tgsieve</sub>"
	}
	return "<sub>" + strings.Join(notes, " · ") + "</sub>"
}

// budget accumulates output and stops adding once the size limit is reached,
// so a report is trimmed deliberately rather than rejected by whatever posts
// it.
type budget struct {
	sb      strings.Builder
	limit   int
	trimmed bool
}

func (b *budget) line(s string) {
	if b.trimmed {
		return
	}
	if b.sb.Len()+len(s)+1 > b.limit {
		b.trimmed = true
		return
	}
	b.sb.WriteString(s)
	b.sb.WriteByte('\n')
}

// forceLine writes past the limit, for the note explaining the limit.
func (b *budget) forceLine(s string) {
	b.sb.WriteString(s)
	b.sb.WriteByte('\n')
}

func (b *budget) String() string { return b.sb.String() }
