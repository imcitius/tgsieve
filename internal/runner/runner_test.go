package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestMeaningfulChangesIgnoresGeneratedArtifacts(t *testing.T) {
	status := strings.Join([]string{
		"?? envs/prod/a/.terragrunt-cache/abc/def/backend.tf",
		" M envs/prod/a/terragrunt.hcl",
		"?? state/envs/prod/a/terraform.tfstate",
		"?? plans/envs/prod/a/tfplan.json",
		"?? envs/prod/b/.terraform/providers/x",
		"?? tfplan.tfplan",
	}, "\n")

	got := meaningfulChanges(status)
	if got != " M envs/prod/a/terragrunt.hcl" {
		t.Errorf("only real configuration changes should count, got:\n%q", got)
	}
}

func TestProvenanceGenerationComparison(t *testing.T) {
	a := Provenance{Commit: "abc", Tree: "1111"}
	same := Provenance{Commit: "abc", Tree: "1111", Created: time.Now()}
	edited := Provenance{Commit: "abc", Tree: "2222"}
	moved := Provenance{Commit: "def", Tree: "1111"}

	if !a.SameGeneration(same) {
		t.Error("identical commit and tree is the same generation")
	}
	if a.SameGeneration(edited) {
		t.Error("an edited working tree is a new generation")
	}
	if a.SameGeneration(moved) {
		t.Error("a different commit is a new generation")
	}
}

func TestFingerprintConfigsTracksContentNotNoise(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("root.hcl", "locals { a = 1 }")
	write("envs/prod/terragrunt.hcl", "inputs = { size = \"small\" }")

	base := fingerprintConfigs(dir)
	if base == "" {
		t.Fatal("a directory with config files must fingerprint to something")
	}

	// Cache and state churn must not move the fingerprint.
	write("envs/prod/.terragrunt-cache/abc/main.tf", "resource \"null_resource\" \"x\" {}")
	write("state/terraform.tfstate", `{"version":4}`)
	if got := fingerprintConfigs(dir); got != base {
		t.Errorf("generated files changed the fingerprint: %s -> %s", base, got)
	}

	// Neither may plans saved inside the working directory.
	write("plans/envs/prod/tfplan.json", `{"format_version":"1.2"}`)
	if got := fingerprintConfigs(dir); got != base {
		t.Errorf("saved plans changed the fingerprint: %s -> %s", base, got)
	}

	// A real edit must.
	write("envs/prod/terragrunt.hcl", "inputs = { size = \"large\" }")
	if got := fingerprintConfigs(dir); got == base {
		t.Error("editing a unit did not change the fingerprint")
	}
}

func TestFingerprintEmptyDirIsEmpty(t *testing.T) {
	if got := fingerprintConfigs(t.TempDir()); got != "" {
		t.Errorf("nothing to fingerprint should be empty, got %q", got)
	}
}

func TestLockIsExclusiveAndReleases(t *testing.T) {
	dir := t.TempDir()

	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err := Lock(dir); err == nil {
		t.Fatal("a second run must not be able to claim the same directory")
	}

	release()
	release2, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()
}

func TestLockTakesOverFromDeadProcess(t *testing.T) {
	dir := t.TempDir()
	// PID 0 is never a live user process, so this stands in for a crashed run.
	stale := `{"pid":0,"started":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, LockFile), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("a crashed run should not need manual cleanup: %v", err)
	}
	release()
}

func TestLockIgnoresGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFile), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("an unreadable lock should be replaced, not fatal: %v", err)
	}
	release()
}

func TestLockRespectsForeignHostUntilStale(t *testing.T) {
	dir := t.TempDir()
	writeLock := func(started time.Time) {
		body := fmt.Sprintf(`{"pid":%d,"started":%q,"host":"another-machine"}`,
			os.Getpid(), started.Format(time.RFC3339Nano))
		if err := os.WriteFile(filepath.Join(dir, LockFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A live pid number means nothing on another host, so a recent foreign
	// lock is respected even though this pid exists here.
	writeLock(time.Now().Add(-time.Minute))
	if _, err := Lock(dir); err == nil {
		t.Fatal("a recent lock from another host must be respected")
	} else if !strings.Contains(err.Error(), "another-machine") {
		t.Errorf("the error should name the host: %v", err)
	}

	// Past the staleness window it is taken over, or the directory would stay
	// locked forever after a crash elsewhere.
	writeLock(time.Now().Add(-staleAfter - time.Minute))
	release, err := Lock(dir)
	if err != nil {
		t.Fatalf("an old foreign lock should be taken over: %v", err)
	}
	release()
}

func TestApplyTimingsIgnoresExpiredMeasurements(t *testing.T) {
	dir := t.TempDir()
	fresh := savedTiming{Millis: 500, Recorded: time.Now().Add(-time.Hour)}
	stale := savedTiming{Millis: 9000, Recorded: time.Now().Add(-timingTTL - time.Hour)}
	body, _ := json.Marshal(map[string]savedTiming{"envs/a": fresh, "envs/b": stale})
	if err := os.WriteFile(filepath.Join(dir, TimingsFile), body, 0o644); err != nil {
		t.Fatal(err)
	}

	run := model.Run{Units: []model.Unit{{Path: "envs/a"}, {Path: "envs/b"}}}
	ApplyTimings(dir, &run)

	if run.Units[0].Duration != 500*time.Millisecond || !run.Units[0].Reused {
		t.Errorf("fresh measurement not applied: %+v", run.Units[0])
	}
	if run.Units[1].Duration != 0 {
		t.Errorf("a measurement older than the TTL must not be reported: %+v", run.Units[1])
	}
}

func TestSourceChangesNamesMovedUnits(t *testing.T) {
	was := Provenance{Commit: "abc", Tree: "1", Sources: map[string]string{
		"envs/prod/a": "git::https://example.com/mod.git//app?ref=v1.0.0",
		"envs/prod/b": "git::https://example.com/mod.git//app?ref=v1.0.0",
		"envs/prod/c": "./modules/app",
	}}
	now := was
	now.Sources = map[string]string{
		"envs/prod/a": "git::https://example.com/mod.git//app?ref=v1.1.0", // moved
		"envs/prod/b": "git::https://example.com/mod.git//app?ref=v1.0.0",
		// c missing: could not be resolved this time
	}

	if got := was.SourceChanges(now); len(got) != 1 || got[0] != "envs/prod/a" {
		t.Errorf("SourceChanges = %v, want [envs/prod/a]", got)
	}
	if was.SameGeneration(now) {
		t.Error("a moved module source is a new generation even with an identical repo")
	}

	// An unresolvable source is unknown, not changed.
	partial := Provenance{Commit: "abc", Tree: "1"}
	if len(was.SourceChanges(partial)) != 0 {
		t.Error("missing source information must not be read as a change")
	}
}
