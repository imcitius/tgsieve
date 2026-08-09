// Package sieve applies noise rules to a run and collapses repeated changes.
package sieve

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/imcitius/tgsieve/internal/config"
	"github.com/imcitius/tgsieve/internal/model"
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
	// Varies marks the individual attribute paths that differ between members,
	// so the ones that agree can still be shown concretely.
	Varies map[string]bool
}

func (g Group) Instances() int { return len(g.Members) }

// IndexLabel summarizes the instance indices behind this group, e.g. "[0-11]".
func (g Group) IndexLabel() string {
	nums := []int{}
	other := []string{}
	for _, m := range g.Members {
		switch v := m.Index.(type) {
		case nil:
			// single instance
		case float64:
			nums = append(nums, int(v))
		case string:
			other = append(other, v)
		}
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

type RuleStat struct {
	Rule  string
	Attrs int
	Res   int
}

// Report is the sieved view of a run, ready to render.
type Report struct {
	Groups         []Group
	ErroredUnits   []model.Unit
	UnchangedUnits []string
	Outputs        map[string][]model.AttrChange

	UnitsTotal   int
	UnitsChanged int

	Raw  model.Counts // before sieving
	Kept model.Counts // after sieving

	HiddenResources int
	HiddenAttrs     int
	RuleStats       []RuleStat
	Explanations    []Explanation
}

func (r Report) HasChanges() bool { return r.Kept.Total() > 0 }

// Apply runs the sieve over a whole run.
func Apply(run model.Run, cfg *config.Config) *Report {
	rep := &Report{Outputs: map[string][]model.AttrChange{}, Raw: run.Counts()}
	ruleIdx := map[string]*RuleStat{}

	hideDrift := cfg.Hide.Drift != nil && *cfg.Hide.Drift
	hideOutputs := cfg.Hide.Outputs != nil && *cfg.Hide.Outputs

	kept := make([]model.Resource, 0, 128)
	for _, u := range run.Units {
		rep.UnitsTotal++
		if u.Errored {
			rep.ErroredUnits = append(rep.ErroredUnits, u)
			continue
		}
		unitKept := 0
		for _, res := range u.Resources {
			if res.Drift && hideDrift {
				continue
			}
			r, dropped := sieveResource(res, cfg, rep, ruleIdx)
			if dropped {
				rep.HiddenResources++
				continue
			}
			kept = append(kept, r)
			rep.Kept.Add(r.Action, r.Drift)
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
	}

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

// sieveResource strips ignored attributes and reports whether the whole
// resource should disappear.
func sieveResource(res model.Resource, cfg *config.Config, rep *Report, stats map[string]*RuleStat) (model.Resource, bool) {
	if cfg.NeverHide.Matches(string(res.Action), res.Type) {
		return res, false
	}
	if len(cfg.Ignore) == 0 {
		return res, false
	}

	matched := make([]int, 0, len(cfg.Ignore))
	for i, rule := range cfg.Ignore {
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
		g.Varies = varyingPaths(g.Members)
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

// varyingPaths finds the attribute paths whose value is not identical across
// every member of a group. Everything else can be printed as a real value
// instead of a useless "varies".
func varyingPaths(members []model.Resource) map[string]bool {
	if len(members) < 2 {
		return nil
	}
	first := map[string]string{}
	for _, a := range members[0].Attrs {
		first[a.Path] = valueKey(a)
	}
	varies := map[string]bool{}
	for _, m := range members[1:] {
		seen := make(map[string]bool, len(m.Attrs))
		for _, a := range m.Attrs {
			seen[a.Path] = true
			if v, ok := first[a.Path]; !ok || v != valueKey(a) {
				varies[a.Path] = true
			}
		}
		for p := range first {
			if !seen[p] {
				varies[p] = true
			}
		}
	}
	return varies
}

func valueKey(a model.AttrChange) string {
	// The prior value counts even when the new one is unknown: "was X, will be
	// computed" and "was Y, will be computed" are not the same story.
	b, _ := json.Marshal([3]any{a.Before, a.After, a.AfterUnknown})
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
