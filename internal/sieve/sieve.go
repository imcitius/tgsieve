// Package sieve applies noise rules to a run and collapses repeated changes.
package sieve

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/imcitius/tgsieve/internal/config"
	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/textutil"
)

// Group is one rendered block: a change that may stand for several instances
// and several units at once.
type Group struct {
	Action  model.Action
	Drift   bool
	Sample  model.Resource
	Members []model.Resource
	Units   []string
	// ValueVary is true when the members do not all carry the same values.
	ValueVary bool
	// Varies marks, per attribute of Sample, whether the members disagree on
	// its value. It is positional rather than keyed by path: a collection can
	// contribute several entries under one path (members added and removed),
	// and keying by path would collapse them into one another.
	Varies []bool
}

func (g Group) Instances() int { return len(g.Members) }

// IndexLabel summarizes the instance indices behind this group, e.g. "[0-11]".
func (g Group) IndexLabel() string {
	// A group can span units, and each of them contributes its own instances.
	// The label describes which indices exist, not how many units have them,
	// so "[2,2,2,2]" for four units holding index 2 is just noise.
	numSet := map[int]bool{}
	otherSet := map[string]bool{}
	for _, m := range g.Members {
		switch v := m.Index.(type) {
		case nil:
			// single instance
		case float64:
			numSet[int(v)] = true
		case string:
			otherSet[v] = true
		}
	}
	nums := make([]int, 0, len(numSet))
	for n := range numSet {
		nums = append(nums, n)
	}
	other := make([]string, 0, len(otherSet))
	for o := range otherSet {
		other = append(other, o)
	}
	sort.Ints(nums)
	parts := []string{}
	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, strconv.Itoa(nums[i])+"-"+strconv.Itoa(nums[j]))
		} else {
			parts = append(parts, strconv.Itoa(nums[i]))
		}
		i = j + 1
	}
	sort.Strings(other)
	if len(other) > 3 {
		other = append(other[:3], "…")
	}
	parts = append(parts, other...)
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, ",") + "]"
}

type Explanation struct {
	Unit    string
	Address string
	Path    string
	Rule    string
}

// SkippedUnit is a unit that never ran: its dependency failed, it was excluded,
// or the run was interrupted before it got a turn. Counting these as
// "unchanged" would claim knowledge nobody has.
type SkippedUnit struct {
	Path   string
	Reason string
}

// FailureGroup collects units that failed for the same reason. A missing
// backend bucket or an expired credential fails every unit in the stack with
// one message, and printing it once per unit buries the fact that there is a
// single thing to fix.
type FailureGroup struct {
	Headline string
	Detail   []string
	Units    []string
	// Variants counts the distinct wordings folded into this group: the same
	// cause reported against different resources or regions.
	Variants int
	// Count is how many times the failure was reported. One broken provider
	// configuration can produce one diagnostic per orphaned resource, and the
	// number is the news, not the repetition.
	Count int
	// Locations are the distinct places the failure was reported from. Five
	// diagnostics sharing a message but naming five different lines are five
	// things to fix, and folding them without the lines would hide the work.
	Locations []string
}

// UnitTiming is how long one unit took, for --timings.
type UnitTiming struct {
	Path     string
	Duration time.Duration
	Changes  int
	// Reused means the duration comes from the run that first planned this
	// unit, not from this invocation.
	Reused bool
	// Age is how old a reused measurement is.
	Age time.Duration
}

type RuleStat struct {
	Rule  string
	Attrs int
	Res   int
}

// Report is the sieved view of a run, ready to render.
type Report struct {
	Groups         []Group
	ErroredUnits   []model.Unit
	Failures       []FailureGroup
	SkippedUnits   []SkippedUnit
	UnchangedUnits []string
	Outputs        map[string][]model.AttrChange

	UnitsTotal   int
	UnitsChanged int

	Raw  model.Counts // before sieving
	Kept model.Counts // after sieving

	HiddenResources int
	HiddenAttrs     int
	// Normalized counts differences dropped by the normalize rules, kept apart
	// from rule-hidden attributes because the reason is different.
	Normalized int
	// ExpiredRules names suppressions that have lapsed and are no longer
	// hiding anything.
	ExpiredRules []string
	RuleStats    []RuleStat
	Explanations []Explanation

	// Timings is every unit that reported a duration, slowest first.
	Timings []UnitTiming
	// Wall is the duration of the whole run, when the caller knows it.
	Wall time.Duration
	// NoRefresh records that the plan was produced without refreshing state.
	NoRefresh bool
	// TFPath is the tofu/terraform binary that ran.
	TFPath string
	// Direct records that terragrunt was not involved, which changes how the
	// binary is described.
	Direct bool
	// Severity is the highest severity among the surviving changes, and
	// SeverityCounts how many changes sit at each level.
	Severity       string
	SeverityCounts map[string]int
}

func (r Report) HasChanges() bool { return r.Kept.Total() > 0 }

// Reads counts the data sources that will be resolved during apply. They are
// not changes and never counted as such, but they are worth showing.
func (r Report) Reads() int {
	n := 0
	for _, g := range r.Groups {
		if g.Action == model.ActionRead {
			n += g.Instances()
		}
	}
	return n
}

// ChangedUnits lists the units with something to do, which is what an apply
// actually works through — the rest of the queue is visited and left alone.
func (r Report) ChangedUnits() []string {
	seen := map[string]bool{}
	for _, g := range r.Groups {
		for _, u := range g.Units {
			seen[u] = true
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// Apply runs the sieve over a whole run.
func Apply(run model.Run, cfg *config.Config) *Report {
	return ApplyAt(run, cfg, time.Now())
}

// ApplyAt is Apply with the clock supplied, so rule expiry can be tested.
func ApplyAt(run model.Run, cfg *config.Config, now time.Time) *Report {
	rep := &Report{Outputs: map[string][]model.AttrChange{}, Raw: run.Counts()}
	ruleIdx := map[string]*RuleStat{}
	for i, rule := range cfg.Ignore {
		if rule.Expired(now) {
			rep.ExpiredRules = append(rep.ExpiredRules, rule.Label(i))
		}
	}

	hideDrift := cfg.Hide.Drift != nil && *cfg.Hide.Drift
	hideOutputs := cfg.Hide.Outputs != nil && *cfg.Hide.Outputs
	hideReads := cfg.Hide.Reads != nil && *cfg.Hide.Reads

	kept := make([]model.Resource, 0, 128)
	for _, u := range run.Units {
		rep.UnitsTotal++
		if u.Errored {
			rep.ErroredUnits = append(rep.ErroredUnits, u)
			continue
		}
		if u.Skipped {
			reason := u.Error
			if reason == "" {
				reason = "not run"
			}
			rep.SkippedUnits = append(rep.SkippedUnits, SkippedUnit{Path: u.Path, Reason: reason})
			continue
		}
		unitKept := 0
		for _, res := range u.Resources {
			if res.Drift && hideDrift {
				continue
			}
			if res.Action == model.ActionRead && hideReads {
				continue
			}
			res, gone := normalizeAttrs(res, cfg, rep)
			if gone {
				continue
			}
			r, dropped := sieveResource(res, cfg, rep, ruleIdx, now)
			if dropped {
				rep.HiddenResources++
				continue
			}
			kept = append(kept, r)
			rep.Kept.Add(r.Action, r.Drift)
			if r.Drift && !r.DriftReverted {
				rep.Kept.DriftLeft++
			}
			unitKept++
		}
		if !hideOutputs && len(u.Outputs) > 0 {
			rep.Outputs[u.Path] = u.Outputs
		}
		if unitKept == 0 && len(rep.Outputs[u.Path]) == 0 {
			rep.UnchangedUnits = append(rep.UnchangedUnits, u.Path)
		} else {
			rep.UnitsChanged++
		}
		if u.Duration > 0 {
			t := UnitTiming{Path: u.Path, Duration: u.Duration, Changes: unitKept, Reused: u.Reused}
			if u.Reused && !u.TimedAt.IsZero() {
				t.Age = time.Since(u.TimedAt)
			}
			rep.Timings = append(rep.Timings, t)
		}
	}
	sort.Slice(rep.Timings, func(i, j int) bool { return rep.Timings[i].Duration > rep.Timings[j].Duration })

	rep.rankSeverity(cfg)
	rep.Failures = groupFailures(rep.ErroredUnits)
	rep.Groups = collapse(kept, cfg)
	for _, s := range ruleIdx {
		rep.RuleStats = append(rep.RuleStats, *s)
	}
	sort.Slice(rep.RuleStats, func(i, j int) bool {
		if rep.RuleStats[i].Attrs != rep.RuleStats[j].Attrs {
			return rep.RuleStats[i].Attrs > rep.RuleStats[j].Attrs
		}
		return rep.RuleStats[i].Rule < rep.RuleStats[j].Rule
	})
	sort.Strings(rep.UnchangedUnits)
	return rep
}

// rankSeverity records how serious the surviving changes are, so a pipeline
// can fail on a replacement without failing on a new log group.
func (r *Report) rankSeverity(cfg *config.Config) {
	r.SeverityCounts = map[string]int{}
	add := func(action string, n int) {
		if n == 0 {
			return
		}
		level := cfg.SeverityOf(action)
		r.SeverityCounts[level] += n
		if config.SeverityRank(level) > config.SeverityRank(r.Severity) {
			r.Severity = level
		}
	}
	add("delete", r.Kept.Delete)
	add("replace", r.Kept.Replace)
	add("update", r.Kept.Update)
	add("create", r.Kept.Create)
	add("drift", r.Kept.Drift)
}

// AtLeast reports whether anything survived at or above a severity level.
func (r Report) AtLeast(level string) bool {
	want := config.SeverityRank(level)
	for got, n := range r.SeverityCounts {
		if n > 0 && config.SeverityRank(got) >= want {
			return true
		}
	}
	return false
}

// normalizeAttrs drops differences the configuration says are not
// differences: empty-versus-null, and reorderings of the same members.
func normalizeAttrs(res model.Resource, cfg *config.Config, rep *Report) (model.Resource, bool) {
	emptyAsNull := cfg.Normalize.EmptyAsNull != nil && *cfg.Normalize.EmptyAsNull
	ignoreReorder := cfg.Normalize.Reorder == "ignore"
	if !emptyAsNull && !ignoreReorder {
		return res, false
	}
	before := len(res.Attrs)
	kept := res.Attrs[:0:0]
	for _, a := range res.Attrs {
		switch {
		case ignoreReorder && a.Kind == model.KindReordered:
		case emptyAsNull && a.Kind == model.KindChanged && !a.AfterUnknown && emptyish(a.Before) && emptyish(a.After):
		default:
			kept = append(kept, a)
			continue
		}
		rep.Normalized++
	}
	res.Attrs = kept
	// A resource whose every difference was normalized away has nothing left
	// to report — unless it is one the safety net protects.
	dropped := before > 0 && len(kept) == 0 && !cfg.NeverHide.Matches(string(res.Action), res.Type)
	return res, dropped
}

// emptyish reports whether a value carries no information: null, "", [] or {}.
func emptyish(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// sieveResource strips ignored attributes and reports whether the whole
// resource should disappear.
func sieveResource(res model.Resource, cfg *config.Config, rep *Report, stats map[string]*RuleStat, now time.Time) (model.Resource, bool) {
	if cfg.NeverHide.Matches(string(res.Action), res.Type) {
		return res, false
	}
	if len(cfg.Ignore) == 0 {
		return res, false
	}

	matched := make([]int, 0, len(cfg.Ignore))
	for i, rule := range cfg.Ignore {
		if rule.Expired(now) {
			continue
		}
		if rule.MatchesResource(res.Unit, res.Type, res.Address, string(res.Action)) {
			matched = append(matched, i)
		}
	}
	if len(matched) == 0 {
		return res, false
	}

	before := len(res.Attrs)
	keptAttrs := res.Attrs[:0:0]
	hitRules := map[string]bool{}
	for _, a := range res.Attrs {
		hiddenBy := ""
		for _, i := range matched {
			if cfg.Ignore[i].MatchesAttr(a.Path) {
				hiddenBy = cfg.Ignore[i].Label(i)
				break
			}
		}
		if hiddenBy == "" {
			keptAttrs = append(keptAttrs, a)
			continue
		}
		if a.ForcesReplace {
			// A rule must never mask the reason a resource is being replaced.
			keptAttrs = append(keptAttrs, a)
			continue
		}
		res.Hidden = append(res.Hidden, model.HiddenAttr{Path: a.Path, Rule: hiddenBy})
		rep.HiddenAttrs++
		rep.Explanations = append(rep.Explanations, Explanation{
			Unit: res.Unit, Address: res.Address, Path: a.Path, Rule: hiddenBy,
		})
		st, ok := stats[hiddenBy]
		if !ok {
			st = &RuleStat{Rule: hiddenBy}
			stats[hiddenBy] = st
		}
		st.Attrs++
		hitRules[hiddenBy] = true
	}
	res.Attrs = keptAttrs

	if before > 0 && len(keptAttrs) == 0 {
		for r := range hitRules {
			stats[r].Res++
		}
		return res, true
	}
	return res, false
}

func collapse(res []model.Resource, cfg *config.Config) []Group {
	instances := cfg.Collapse.Instances == nil || *cfg.Collapse.Instances
	crossUnit := cfg.Collapse.CrossUnit == nil || *cfg.Collapse.CrossUnit
	strict := cfg.Collapse.CrossUnitMode == "strict"
	minUnits := cfg.Collapse.MinUnits
	if minUnits <= 0 {
		minUnits = 2
	}

	type bucket struct {
		g     *Group
		order int
	}
	buckets := map[string]*bucket{}
	order := 0

	for _, r := range res {
		var key strings.Builder
		key.WriteString(string(r.Action))
		key.WriteByte(0)
		if r.Drift {
			key.WriteString("drift")
			if r.DriftReverted {
				key.WriteString("-reverted")
			}
		}
		key.WriteByte(0)
		key.WriteString(r.Module + "|" + r.Type + "|" + r.Name)
		key.WriteByte(0)
		if !instances {
			key.WriteString(r.Address)
			key.WriteByte(0)
		}
		if !crossUnit {
			key.WriteString(r.Unit)
			key.WriteByte(0)
		}
		if strict {
			key.WriteString(r.ValueShape())
		} else {
			key.WriteString(r.Shape())
		}

		b, ok := buckets[key.String()]
		if !ok {
			g := &Group{Action: r.Action, Drift: r.Drift, Sample: r}
			b = &bucket{g: g, order: order}
			order++
			buckets[key.String()] = b
		}
		b.g.Members = append(b.g.Members, r)
	}

	out := make([]Group, 0, len(buckets))
	for _, b := range buckets {
		g := b.g
		unitSet := map[string]bool{}
		shapes := map[string]bool{}
		for _, m := range g.Members {
			unitSet[m.Unit] = true
			shapes[m.ValueShape()] = true
		}
		g.Units = keys(unitSet)
		g.ValueVary = len(shapes) > 1
		g.Varies = varyingAttrs(g.Members)
		// Cross-unit collapse only pays off past the configured threshold;
		// below it, split back into per-unit groups so paths stay concrete.
		if crossUnit && len(g.Units) > 1 && len(g.Units) < minUnits {
			out = append(out, splitByUnit(*g)...)
			continue
		}
		out = append(out, *g)
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Drift != b.Drift {
			return !a.Drift
		}
		if sa, sb := a.Action.Severity(), b.Action.Severity(); sa != sb {
			return sa > sb
		}
		if len(a.Units) != len(b.Units) {
			return len(a.Units) > len(b.Units)
		}
		if a.Sample.Unit != b.Sample.Unit {
			return a.Sample.Unit < b.Sample.Unit
		}
		return a.Sample.Address < b.Sample.Address
	})
	return out
}

// groupFailures folds units that failed with the same headline together,
// preserving the order in which they first appeared.
func groupFailures(units []model.Unit) []FailureGroup {
	var out []FailureGroup
	index := map[string]int{}
	wordings := map[int]map[string]bool{}
	unitsIn := map[int]map[string]bool{}
	seenLoc := map[int]map[string]bool{}
	for _, u := range units {
		for _, msg := range unitErrors(u) {
			head := textutil.Headline(msg)
			if head == "" {
				head = "failed"
			}
			key := textutil.NormalizeError(head)
			i, ok := index[key]
			if !ok {
				i = len(out)
				index[key] = i
				wordings[i] = map[string]bool{}
				unitsIn[i] = map[string]bool{}
				seenLoc[i] = map[string]bool{}
				out = append(out, FailureGroup{
					Headline: head,
					Detail:   textutil.CleanError(msg, 8),
				})
			}
			if !unitsIn[i][u.Path] {
				unitsIn[i][u.Path] = true
				out[i].Units = append(out[i].Units, u.Path)
			}
			if loc := textutil.Location(msg); loc != "" && !seenLoc[i][loc] {
				if seenLoc[i] == nil {
					seenLoc[i] = map[string]bool{}
				}
				seenLoc[i][loc] = true
				out[i].Locations = append(out[i].Locations, loc)
			}
			out[i].Count++
			wordings[i][head] = true
			out[i].Variants = len(wordings[i])
		}
	}
	for i := range out {
		sortLocations(out[i].Locations)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Units) > len(out[j].Units) })
	return out
}

// sortLocations orders file:line references the way someone reads a file:
// grouped by name, ascending by line.
func sortLocations(locs []string) {
	sort.SliceStable(locs, func(i, j int) bool {
		fi, li := splitLocation(locs[i])
		fj, lj := splitLocation(locs[j])
		if fi != fj {
			return fi < fj
		}
		return li < lj
	})
}

func splitLocation(loc string) (string, int) {
	i := strings.LastIndex(loc, ":")
	if i < 0 {
		return loc, 0
	}
	n, err := strconv.Atoi(loc[i+1:])
	if err != nil {
		return loc, 0
	}
	return loc[:i], n
}

// unitErrors returns every diagnostic recorded for a unit, falling back to the
// single message when that is all there is.
func unitErrors(u model.Unit) []string {
	if len(u.Errors) > 0 {
		return u.Errors
	}
	if u.Error != "" {
		return []string{u.Error}
	}
	return []string{"failed"}
}

// varyingAttrs reports, for each attribute of the first member, whether the
// other members disagree about it. Everything else can be printed as a real
// value instead of a useless "varies".
func varyingAttrs(members []model.Resource) []bool {
	if len(members) < 2 {
		return nil
	}
	first := members[0].Attrs
	varies := make([]bool, len(first))
	for _, m := range members[1:] {
		if len(m.Attrs) != len(first) {
			// Different shape entirely: nothing can be claimed about values.
			for i := range varies {
				varies[i] = true
			}
			return varies
		}
		for i, a := range m.Attrs {
			if a.Path != first[i].Path || valueKey(a) != valueKey(first[i]) {
				varies[i] = true
			}
		}
	}
	return varies
}

func valueKey(a model.AttrChange) string {
	// The prior value counts even when the new one is unknown: "was X, will be
	// computed" and "was Y, will be computed" are not the same story.
	b, _ := json.Marshal([5]any{a.Before, a.After, a.AfterUnknown, a.Kind, a.Count})
	return string(b)
}

func splitByUnit(g Group) []Group {
	byUnit := map[string][]model.Resource{}
	for _, m := range g.Members {
		byUnit[m.Unit] = append(byUnit[m.Unit], m)
	}
	out := make([]Group, 0, len(byUnit))
	for _, u := range keys(toSet(byUnit)) {
		members := byUnit[u]
		shapes := map[string]bool{}
		for _, m := range members {
			shapes[m.ValueShape()] = true
		}
		out = append(out, Group{
			Action: g.Action, Drift: g.Drift, Sample: members[0],
			Members: members, Units: []string{u}, ValueVary: len(shapes) > 1,
		})
	}
	return out
}

func toSet[T any](m map[string]T) map[string]bool {
	s := make(map[string]bool, len(m))
	for k := range m {
		s[k] = true
	}
	return s
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
