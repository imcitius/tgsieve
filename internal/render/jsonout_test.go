package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/sieve"
)

func renderJSON(t *testing.T, rep *sieve.Report, applied *Applied) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := JSON(&buf, rep, Meta{Version: "1.2.3", Command: "plan", Engine: "terragrunt"}, applied); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	return doc
}

func TestJSONCarriesSchemaAndSummary(t *testing.T) {
	rep := &sieve.Report{
		Kept:         model.Counts{Update: 2, Delete: 1},
		UnitsTotal:   4,
		UnitsChanged: 2,
		Severity:     "high",
		Groups:       []sieve.Group{group(model.ActionDelete, "envs/prod/db", "aws_db_instance.main")},
	}
	doc := renderJSON(t, rep, nil)

	if doc["schema_version"].(float64) != float64(SchemaVersion) {
		t.Errorf("schema_version = %v", doc["schema_version"])
	}
	tool := doc["tool"].(map[string]any)
	if tool["version"] != "1.2.3" || tool["engine"] != "terragrunt" {
		t.Errorf("tool = %v", tool)
	}
	summary := doc["summary"].(map[string]any)
	counts := summary["changes"].(map[string]any)
	if counts["total"].(float64) != 3 {
		t.Errorf("changes.total = %v, want 3", counts["total"])
	}
	if summary["severity"].(map[string]any)["level"] != "high" {
		t.Errorf("severity = %v", summary["severity"])
	}
}

func TestJSONNeverCarriesSensitiveValues(t *testing.T) {
	// A machine-readable report is the easiest place for a secret to travel
	// somewhere it should not.
	rep := &sieve.Report{
		Kept: model.Counts{Update: 1}, UnitsTotal: 1, UnitsChanged: 1,
		Groups: []sieve.Group{group(model.ActionUpdate, "u", "aws_secret.x",
			model.AttrChange{Path: "value", Before: "hunter2", After: "hunter3", Sensitive: true},
			model.AttrChange{Path: "name", Before: "a", After: "b"})},
	}
	var buf bytes.Buffer
	if err := JSON(&buf, rep, Meta{Command: "plan"}, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "hunter2") || strings.Contains(out, "hunter3") {
		t.Fatalf("a sensitive value reached the JSON output:\n%s", out)
	}
	if !strings.Contains(out, `"sensitive": true`) {
		t.Errorf("the attribute should still be reported, marked sensitive:\n%s", out)
	}
	if !strings.Contains(out, `"before": "a"`) {
		t.Errorf("non-sensitive values should still be present:\n%s", out)
	}
}

func TestJSONReportsAnApply(t *testing.T) {
	rep := &sieve.Report{Kept: model.Counts{Update: 1}, UnitsTotal: 1, UnitsChanged: 1}
	doc := renderJSON(t, rep, AppliedResult(true, 1500, []string{"envs/prod/a"}, nil))

	applied := doc["applied"].(map[string]any)
	if applied["ok"] != true || applied["duration_ms"].(float64) != 1500 {
		t.Errorf("applied = %v", applied)
	}
}

func TestJSONListsFailuresWithLocations(t *testing.T) {
	rep := &sieve.Report{
		UnitsTotal:   1,
		ErroredUnits: []model.Unit{{Path: "u", Errored: true}},
		Failures: []sieve.FailureGroup{{
			Headline: "Error: Unsupported attribute", Units: []string{"u"}, Count: 5,
			Locations: []string{"main.tf:16", "main.tf:17"}, Detail: []string{"Error: Unsupported attribute"},
		}},
	}
	doc := renderJSON(t, rep, nil)
	failures := doc["failures"].([]any)
	if len(failures) != 1 {
		t.Fatalf("failures = %v", failures)
	}
	f := failures[0].(map[string]any)
	if f["count"].(float64) != 5 || len(f["locations"].([]any)) != 2 {
		t.Errorf("failure = %v", f)
	}
}
