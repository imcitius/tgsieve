package sieve

import (
	"testing"

	"github.com/imcitius/tgsieve/internal/config"
	"github.com/imcitius/tgsieve/internal/model"
)

func cfgWith(t *testing.T, rules ...config.Rule) *config.Config {
	t.Helper()
	c := config.Default()
	c.Ignore = rules
	if err := config.Compile(c); err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}

func res(unit, addr, typ, name string, act model.Action, attrs ...model.AttrChange) model.Resource {
	return model.Resource{
		Unit: unit, Address: addr, BaseAddress: addr, Type: typ, Name: name,
		Action: act, Attrs: attrs,
	}
}

func attr(path string, before, after any) model.AttrChange {
	return model.AttrChange{Path: path, Before: before, After: after}
}

func TestRuleHidesAttributeButKeepsResource(t *testing.T) {
	cfg := cfgWith(t, config.Rule{Name: "tags", Attrs: []string{"tags.*"}})
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
		res("a", "aws_x.y", "aws_x", "y", model.ActionUpdate,
			attr("tags.ts", "1", "2"), attr("size", "small", "large")),
	}}}}

	rep := Apply(run, cfg)
	if len(rep.Groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(rep.Groups))
	}
	if got := len(rep.Groups[0].Sample.Attrs); got != 1 {
		t.Fatalf("want 1 surviving attr, got %d", got)
	}
	if rep.HiddenAttrs != 1 {
		t.Errorf("HiddenAttrs = %d, want 1", rep.HiddenAttrs)
	}
}

func TestResourceDisappearsWhenEveryAttrIsHidden(t *testing.T) {
	cfg := cfgWith(t, config.Rule{Name: "tags", Attrs: []string{"tags.*"}})
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
		res("a", "aws_x.y", "aws_x", "y", model.ActionUpdate, attr("tags.ts", "1", "2")),
	}}}}

	rep := Apply(run, cfg)
	if len(rep.Groups) != 0 {
		t.Fatalf("resource should be gone, got %d groups", len(rep.Groups))
	}
	if rep.HiddenResources != 1 {
		t.Errorf("HiddenResources = %d, want 1", rep.HiddenResources)
	}
	if len(rep.UnchangedUnits) != 1 {
		t.Errorf("unit should count as unchanged, got %v", rep.UnchangedUnits)
	}
	if rep.HasChanges() {
		t.Error("report should have no surviving changes")
	}
}

func TestNeverHideProtectsDestroy(t *testing.T) {
	cfg := cfgWith(t, config.Rule{Name: "everything", Attrs: []string{"*"}})
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
		res("a", "aws_db.x", "aws_db", "x", model.ActionDelete, attr("name", "db", nil)),
		res("a", "aws_x.y", "aws_x", "y", model.ActionUpdate, attr("size", "s", "l")),
	}}}}

	rep := Apply(run, cfg)
	if len(rep.Groups) != 1 {
		t.Fatalf("want only the destroy to survive, got %d groups", len(rep.Groups))
	}
	if rep.Groups[0].Action != model.ActionDelete {
		t.Errorf("survivor = %v, want delete", rep.Groups[0].Action)
	}
}

func TestForcesReplaceAttrSurvivesRules(t *testing.T) {
	cfg := cfgWith(t, config.Rule{Name: "all", Attrs: []string{"*"}})
	r := res("a", "aws_db.x", "aws_db", "x", model.ActionUpdate, attr("engine", "14", "15"))
	r.Attrs[0].ForcesReplace = true
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{r}}}}

	rep := Apply(run, cfg)
	if len(rep.Groups) != 1 || len(rep.Groups[0].Sample.Attrs) != 1 {
		t.Fatal("an attribute that forces replacement must never be hidden")
	}
}

func TestCollapseInstances(t *testing.T) {
	cfg := config.Default()
	var rs []model.Resource
	for i := 0; i < 12; i++ {
		r := res("a", "null_resource.this", "null_resource", "this", model.ActionCreate, attr("x", nil, "1"))
		r.Index = float64(i)
		rs = append(rs, r)
	}
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: rs}}}

	rep := Apply(run, cfg)
	if len(rep.Groups) != 1 {
		t.Fatalf("12 identical instances should collapse to 1 group, got %d", len(rep.Groups))
	}
	if rep.Groups[0].Instances() != 12 {
		t.Errorf("Instances() = %d, want 12", rep.Groups[0].Instances())
	}
	if got := rep.Groups[0].IndexLabel(); got != "[0-11]" {
		t.Errorf("IndexLabel() = %q, want [0-11]", got)
	}
	if rep.Kept.Create != 12 {
		t.Errorf("counts must stay honest: Create = %d, want 12", rep.Kept.Create)
	}
}

func TestCollapseCrossUnitShapeVsStrict(t *testing.T) {
	mk := func(unit, after string) model.Unit {
		return model.Unit{Path: unit, Resources: []model.Resource{
			res(unit, "aws_x.y", "aws_x", "y", model.ActionUpdate, attr("size", "small", after)),
		}}
	}
	run := model.Run{Units: []model.Unit{mk("envs/a", "large"), mk("envs/b", "huge"), mk("envs/c", "large")}}

	shape := Apply(run, config.Default())
	if len(shape.Groups) != 1 {
		t.Fatalf("shape mode should merge all 3 units, got %d groups", len(shape.Groups))
	}
	if !shape.Groups[0].ValueVary {
		t.Error("ValueVary should be set when the values differ")
	}
	if len(shape.Groups[0].Units) != 3 {
		t.Errorf("Units = %v, want 3", shape.Groups[0].Units)
	}

	strictCfg := config.Default()
	strictCfg.Collapse.CrossUnitMode = "strict"
	strict := Apply(run, strictCfg)
	if len(strict.Groups) != 2 {
		t.Fatalf("strict mode should keep the odd value apart: got %d groups", len(strict.Groups))
	}
}

func TestDriftGoesToItsOwnBucket(t *testing.T) {
	r := res("a", "aws_x.y", "aws_x", "y", model.ActionUpdate, attr("size", "s", "l"))
	r.Drift = true
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{r}}}}

	rep := Apply(run, config.Default())
	if rep.Kept.Drift != 1 || rep.Kept.Update != 0 {
		t.Errorf("drift miscounted: %+v", rep.Kept)
	}

	hide := true
	cfg := config.Default()
	cfg.Hide.Drift = &hide
	if got := Apply(run, cfg); len(got.Groups) != 0 {
		t.Error("hide.drift should remove drift entirely")
	}
}

func TestSkippedUnitsAreNotCountedAsUnchanged(t *testing.T) {
	run := model.Run{Units: []model.Unit{
		{Path: "a", Skipped: true, Error: "skipped: dependency failed"},
		{Path: "b"},
	}}

	rep := Apply(run, config.Default())
	if len(rep.SkippedUnits) != 1 || rep.SkippedUnits[0].Path != "a" {
		t.Fatalf("skipped units = %+v", rep.SkippedUnits)
	}
	if got := rep.UnchangedUnits; len(got) != 1 || got[0] != "b" {
		t.Errorf("only the unit that actually ran is unchanged, got %v", got)
	}
	if rep.SkippedUnits[0].Reason != "skipped: dependency failed" {
		t.Errorf("reason lost: %q", rep.SkippedUnits[0].Reason)
	}
}

func TestFailuresGroupByRootCause(t *testing.T) {
	boom := "Error: no valid credential sources found\ndetail line"
	other := "Error: Unsupported argument\nsomething"
	run := model.Run{Units: []model.Unit{
		{Path: "a", Errored: true, Error: boom},
		{Path: "b", Errored: true, Error: other},
		{Path: "c", Errored: true, Error: boom},
		{Path: "d", Errored: true, Error: boom},
	}}

	rep := Apply(run, config.Default())
	if len(rep.ErroredUnits) != 4 {
		t.Fatalf("every failure still counts: %d", len(rep.ErroredUnits))
	}
	if len(rep.Failures) != 2 {
		t.Fatalf("want 2 root causes, got %d: %+v", len(rep.Failures), rep.Failures)
	}
	// Biggest group first: one fix unblocks three units.
	if got := rep.Failures[0].Units; len(got) != 3 {
		t.Errorf("largest group = %v", got)
	}
	if rep.Failures[0].Headline != "Error: no valid credential sources found" {
		t.Errorf("headline = %q", rep.Failures[0].Headline)
	}
	if len(rep.Failures[0].Detail) == 0 {
		t.Error("the group kept no detail to act on")
	}
}

func TestNormalizeEmptyAsNullIsOptIn(t *testing.T) {
	mk := func() model.Run {
		return model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
			res("a", "aws_x.y", "aws_x", "y", model.ActionUpdate,
				attr("description", "", nil),
				attr("size", "small", "large")),
		}}}}
	}

	// Default: nothing is normalized, because "" -> null is someone else's
	// judgement call to make.
	off := Apply(mk(), config.Default())
	if len(off.Groups) != 1 || len(off.Groups[0].Sample.Attrs) != 2 {
		t.Fatalf("without config both attrs survive: %+v", off.Groups)
	}
	if off.Normalized != 0 {
		t.Errorf("Normalized = %d, want 0", off.Normalized)
	}

	on := config.Default()
	yes := true
	on.Normalize.EmptyAsNull = &yes
	rep := Apply(mk(), on)
	if len(rep.Groups) != 1 || len(rep.Groups[0].Sample.Attrs) != 1 {
		t.Fatalf("empty-to-null should drop, leaving the real change: %+v", rep.Groups)
	}
	if rep.Groups[0].Sample.Attrs[0].Path != "size" {
		t.Errorf("wrong attribute survived: %+v", rep.Groups[0].Sample.Attrs)
	}
	if rep.Normalized != 1 {
		t.Errorf("Normalized = %d, want 1", rep.Normalized)
	}
}

func TestNormalizeReorderIgnore(t *testing.T) {
	reordered := model.AttrChange{Path: "cidrs", Kind: model.KindReordered, Count: 4}
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
		res("a", "aws_x.y", "aws_x", "y", model.ActionUpdate, reordered),
	}}}}

	shown := Apply(run, config.Default())
	if len(shown.Groups) != 1 {
		t.Fatalf("by default a reordering is still reported: %+v", shown.Groups)
	}

	cfg := config.Default()
	cfg.Normalize.Reorder = "ignore"
	hidden := Apply(run, cfg)
	if len(hidden.Groups) != 0 {
		t.Errorf("reorder: ignore should remove it entirely: %+v", hidden.Groups)
	}
	if hidden.Normalized != 1 {
		t.Errorf("Normalized = %d, want 1", hidden.Normalized)
	}
}

func TestNormalizeDoesNotTouchUnknownValues(t *testing.T) {
	// "" -> (known after apply) is not an empty-to-null difference; the new
	// value is simply not known yet.
	a := model.AttrChange{Path: "arn", Before: "", AfterUnknown: true}
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
		res("a", "aws_x.y", "aws_x", "y", model.ActionUpdate, a),
	}}}}
	cfg := config.Default()
	yes := true
	cfg.Normalize.EmptyAsNull = &yes

	rep := Apply(run, cfg)
	if len(rep.Groups) != 1 {
		t.Fatalf("an unknown value must survive normalization: %+v", rep.Groups)
	}
}

func TestSeverityRankingAndFailOn(t *testing.T) {
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
		res("a", "aws_x.new", "aws_x", "new", model.ActionCreate, attr("size", nil, "s")),
		res("a", "aws_x.edit", "aws_x", "edit", model.ActionUpdate, attr("size", "s", "l")),
	}}}}

	rep := Apply(run, config.Default())
	if rep.Severity != "medium" {
		t.Errorf("an update is the worst thing here: %q", rep.Severity)
	}
	if !rep.AtLeast("low") || !rep.AtLeast("medium") {
		t.Error("medium changes satisfy both low and medium thresholds")
	}
	if rep.AtLeast("high") {
		t.Error("nothing here is high")
	}

	// A destroy raises it.
	run.Units[0].Resources = append(run.Units[0].Resources,
		res("a", "aws_x.gone", "aws_x", "gone", model.ActionDelete, attr("size", "s", nil)))
	if rep := Apply(run, config.Default()); !rep.AtLeast("high") || rep.Severity != "high" {
		t.Errorf("a destroy is high: %q %v", rep.Severity, rep.SeverityCounts)
	}
}

func TestSeverityIsConfigurable(t *testing.T) {
	// A team that treats every creation as worth stopping for.
	cfg := config.Default()
	cfg.Severity = map[string]string{"create": "high"}
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
		res("a", "aws_x.new", "aws_x", "new", model.ActionCreate, attr("size", nil, "s")),
	}}}}

	rep := Apply(run, cfg)
	if !rep.AtLeast("high") {
		t.Errorf("severity override ignored: %q %v", rep.Severity, rep.SeverityCounts)
	}
}

func TestSeverityOnlyCountsSurvivingChanges(t *testing.T) {
	// A destroy hidden by rules would still be high — but never_hide protects
	// destroys, so use an update the rules do remove.
	cfg := config.Default()
	cfg.Ignore = []config.Rule{{Name: "all", Attrs: []string{"*"}}}
	if err := config.Compile(cfg); err != nil {
		t.Fatal(err)
	}
	run := model.Run{Units: []model.Unit{{Path: "a", Resources: []model.Resource{
		res("a", "aws_x.edit", "aws_x", "edit", model.ActionUpdate, attr("size", "s", "l")),
	}}}}

	rep := Apply(run, cfg)
	if rep.AtLeast("low") {
		t.Errorf("a change removed by the sieve must not fail a pipeline: %v", rep.SeverityCounts)
	}
}
