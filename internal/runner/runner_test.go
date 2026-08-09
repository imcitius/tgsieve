package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/imcitius/tgsieve/internal/model"
)

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) }

func TestSplitOutArgHonoursCallerPlanFile(t *testing.T) {
	cases := []struct {
		in       []string
		wantRest []string
		wantOut  string
	}{
		{[]string{"-refresh=false"}, []string{"-refresh=false"}, ""},
		{[]string{"-out=my.tfplan"}, nil, "my.tfplan"},
		{[]string{"-out", "my.tfplan", "-lock=false"}, []string{"-lock=false"}, "my.tfplan"},
		{[]string{"--out=my.tfplan", "-parallelism=2"}, []string{"-parallelism=2"}, "my.tfplan"},
	}
	for _, c := range cases {
		rest, out := splitOutArg(c.in)
		if !reflect.DeepEqual(rest, c.wantRest) {
			t.Errorf("splitOutArg(%v) rest = %v, want %v", c.in, rest, c.wantRest)
		}
		if c.wantOut == "" {
			if out != "" {
				t.Errorf("splitOutArg(%v) out = %q, want empty", c.in, out)
			}
			continue
		}
		want, _ := filepath.Abs(c.wantOut)
		if out != want {
			t.Errorf("splitOutArg(%v) out = %q, want %q", c.in, out, want)
		}
	}
}

func TestUnitNameIsProjectRelative(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "envs", "prod", "a")
	if err := mkdirAll(deep); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := unitName(deep); got != "envs/prod/a" {
		t.Errorf("unitName = %q, want envs/prod/a", got)
	}
}

func TestFilterArgs(t *testing.T) {
	o := Options{Filters: []string{"envs/prod/*", "envs/stage/*"}, FilterAffected: true}
	want := []string{"--filter", "envs/prod/*", "--filter", "envs/stage/*", "--filter-affected"}
	if got := o.filterArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("filterArgs() = %v, want %v", got, want)
	}
	if got := (Options{}).filterArgs(); got != nil {
		t.Errorf("no filters should produce no args, got %v", got)
	}
}

func TestProgressLabel(t *testing.T) {
	p := NewProgress(os.Stderr, false, false)
	p.planned = 7
	if got := p.progressLabel(); got != "7 planned" {
		t.Errorf("without a known total: %q", got)
	}
	p.SetTotal(28)
	if got := p.progressLabel(); got != "7/28 planned" {
		t.Errorf("with a known total: %q", got)
	}
}

func writeReport(t *testing.T, entries string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, []byte(entries), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestApplyReportSingleUnitDoesNotDuplicate(t *testing.T) {
	// terragrunt names a single-unit run after its directory alone, while the
	// plan is filed under the project-relative path.
	report := writeReport(t, `[{"Name":"b","Result":"succeeded",
	  "Started":"2026-08-09T10:00:00.0Z","Ended":"2026-08-09T10:00:02.0Z","Cmd":"plan"}]`)
	run := model.Run{Units: []model.Unit{{Path: "envs/prod/b"}}}

	applyReport(&run, report, nil, false)

	if len(run.Units) != 1 {
		t.Fatalf("single-unit run should stay one unit, got %d: %+v", len(run.Units), run.Units)
	}
	if run.Units[0].Duration != 2*time.Second {
		t.Errorf("duration = %v, want 2s", run.Units[0].Duration)
	}
}

func TestApplyReportStackSynthesizesFailedUnits(t *testing.T) {
	report := writeReport(t, `[
	  {"Name":"envs/prod/a","Result":"succeeded","Started":"2026-08-09T10:00:00.0Z","Ended":"2026-08-09T10:00:01.0Z"},
	  {"Name":"envs/prod/b","Result":"failed","Started":"2026-08-09T10:00:00.0Z","Ended":"2026-08-09T10:00:01.0Z"},
	  {"Name":"envs/prod/c","Result":"early exit","Started":"2026-08-09T10:00:00.0Z","Ended":"2026-08-09T10:00:00.0Z"}
	]`)
	run := model.Run{Units: []model.Unit{{Path: "envs/prod/a"}}}

	applyReport(&run, report, []string{"envs/prod/b: Error: boom"}, true)

	if len(run.Units) != 3 {
		t.Fatalf("units = %d, want 3", len(run.Units))
	}
	byPath := map[string]model.Unit{}
	for _, u := range run.Units {
		byPath[u.Path] = u
	}
	if b := byPath["envs/prod/b"]; !b.Errored || b.Error == "" {
		t.Errorf("failed unit not recorded: %+v", b)
	}
	if c := byPath["envs/prod/c"]; !c.Skipped || c.Errored {
		t.Errorf("a unit that never ran is skipped, not failed: %+v", c)
	}
}

func TestHandleTFEventCountsProgressAndErrors(t *testing.T) {
	p := NewProgress(os.Stderr, false, false)
	p.SetTotal(1)
	res := &Result{}
	opts := Options{Progress: p}

	lines := []string{
		`{"@level":"info","@message":"Terraform 1.15.5","type":"version"}`,
		`{"@level":"info","type":"refresh_complete","hook":{"resource":{"addr":"null_resource.a"}}}`,
		`{"@level":"info","type":"refresh_complete","hook":{"resource":{"addr":"null_resource.b"}}}`,
		`{"@level":"info","type":"planned_change","change":{"resource":{"addr":"null_resource.a"},"action":"update"}}`,
		`{"@level":"error","type":"diagnostic","diagnostic":{"severity":"error","summary":"Invalid value","detail":"a number is required"}}`,
	}
	for _, l := range lines {
		if !handleTFEvent(l, opts, res) {
			t.Fatalf("not recognised as a terraform event: %s", l)
		}
	}
	if handleTFEvent(`{"level":"info","msg":"terragrunt line"}`, opts, res) {
		t.Error("a terragrunt log record is not a terraform event")
	}
	if handleTFEvent("not json at all", opts, res) {
		t.Error("garbage is not a terraform event")
	}

	if p.refreshed != 2 || p.resources != 1 {
		t.Errorf("counters: refreshed=%d resources=%d, want 2 and 1", p.refreshed, p.resources)
	}
	if got := p.progressLabel(); got != "2 resources refreshed · 1 to change" {
		t.Errorf("label = %q", got)
	}
	if len(res.Errors) != 1 || res.Errors[0] != "Invalid value: a number is required" {
		t.Errorf("diagnostics not captured: %v", res.Errors)
	}
}

func TestProgressLabelShowsRunningUnits(t *testing.T) {
	p := NewProgress(os.Stderr, false, false)
	p.SetTotal(5)
	p.Unit("envs/prod/a")
	p.Unit("envs/prod/b")
	p.planned = 1
	if got := p.progressLabel(); got != "1/5 planned · 1 running" {
		t.Errorf("label = %q", got)
	}
}
