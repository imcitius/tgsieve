package tfplan

import (
	"encoding/json"
	"testing"

	"github.com/imcitius/tgsieve/internal/model"
)

func changeFrom(t *testing.T, raw string) Change {
	t.Helper()
	var c Change
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return c
}

func attrMap(attrs []model.AttrChange) map[string]model.AttrChange {
	m := make(map[string]model.AttrChange, len(attrs))
	for _, a := range attrs {
		m[a.Path] = a
	}
	return m
}

func TestDiffUpdateFindsOnlyChangedPaths(t *testing.T) {
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"name": "app", "desired_count": 2, "tags": {"env": "prod", "ts": "2026-01-01"}},
	  "after":  {"name": "app", "desired_count": 4, "tags": {"env": "prod", "ts": "2026-02-02"}},
	  "after_unknown": {"arn": true},
	  "before_sensitive": {},
	  "after_sensitive": {}
	}`)

	got := attrMap(Diff(ch))
	if len(got) != 3 {
		t.Fatalf("want 3 changed attrs, got %d: %v", len(got), got)
	}
	if _, ok := got["name"]; ok {
		t.Error("unchanged attribute 'name' leaked into the diff")
	}
	if a := got["desired_count"]; a.Before != float64(2) || a.After != float64(4) {
		t.Errorf("desired_count: got %v -> %v", a.Before, a.After)
	}
	if a := got["arn"]; !a.AfterUnknown {
		t.Error("arn should be marked known-after-apply")
	}
	if _, ok := got["tags.ts"]; !ok {
		t.Error("nested tags.ts change missing")
	}
}

func TestDiffMarksSensitiveAndUnknownSubtrees(t *testing.T) {
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"secret": "old", "conf": {"a": 1, "b": 2}},
	  "after":  {"secret": "new", "conf": {"a": 1, "b": 3}},
	  "after_unknown": {"conf": true},
	  "before_sensitive": {"secret": true},
	  "after_sensitive": {"secret": true}
	}`)

	got := attrMap(Diff(ch))
	if !got["secret"].Sensitive {
		t.Error("secret should be sensitive")
	}
	// The whole "conf" subtree is unknown, so it is reported once at the
	// outermost path instead of once per child.
	if !got["conf"].AfterUnknown {
		t.Error("conf should be reported as known-after-apply")
	}
	if _, ok := got["conf.b"]; ok {
		t.Error("children of an unknown subtree must not be repeated")
	}
	if b, ok := got["conf"].Before.(map[string]any); !ok || b["b"] != float64(2) {
		t.Errorf("conf.Before should carry the prior subtree, got %#v", got["conf"].Before)
	}
}

func TestDiffListIndexesUseNaturalOrder(t *testing.T) {
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"ports": [80, 443, 8080, 8443, 9000, 9090, 9100, 9200, 9300, 9400, 9500]},
	  "after":  {"ports": [81, 444, 8081, 8444, 9001, 9091, 9101, 9201, 9301, 9401, 9501]}
	}`)

	attrs := Diff(ch)
	if attrs[0].Path != "ports.0" {
		t.Fatalf("first path = %q", attrs[0].Path)
	}
	if attrs[2].Path != "ports.2" || attrs[10].Path != "ports.10" {
		t.Fatalf("paths not in natural order: %q ... %q", attrs[2].Path, attrs[10].Path)
	}
}

func TestActionMapping(t *testing.T) {
	cases := []struct {
		in   []string
		want model.Action
	}{
		{[]string{"no-op"}, model.ActionNoOp},
		{[]string{"create"}, model.ActionCreate},
		{[]string{"update"}, model.ActionUpdate},
		{[]string{"delete"}, model.ActionDelete},
		{[]string{"delete", "create"}, model.ActionReplace},
		{[]string{"create", "delete"}, model.ActionReplace},
	}
	for _, c := range cases {
		if got := actionOf(c.in); got != c.want {
			t.Errorf("actionOf(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReplacePathsMarkAttributes(t *testing.T) {
	rc := ResourceChange{
		Address: "aws_db_instance.main",
		Type:    "aws_db_instance",
		Name:    "main",
		Change: changeFrom(t, `{
		  "actions": ["delete", "create"],
		  "before": {"engine_version": "14.7", "size": "small"},
		  "after":  {"engine_version": "15.3", "size": "large"},
		  "replace_paths": [["engine_version"]]
		}`),
	}
	r, ok := toResource("envs/prod", rc, false)
	if !ok {
		t.Fatal("resource dropped")
	}
	if r.Action != model.ActionReplace {
		t.Fatalf("action = %v", r.Action)
	}
	got := attrMap(r.Attrs)
	if !got["engine_version"].ForcesReplace {
		t.Error("engine_version should be marked as forcing replacement")
	}
	if got["size"].ForcesReplace {
		t.Error("size does not force replacement")
	}
}

func TestNoOpResourcesAreDropped(t *testing.T) {
	rc := ResourceChange{
		Address: "null_resource.x",
		Change:  changeFrom(t, `{"actions": ["no-op"], "before": {"a": 1}, "after": {"a": 1}}`),
	}
	if _, ok := toResource("u", rc, false); ok {
		t.Error("no-op resource should not enter the model")
	}
}

func TestBaseAddressStripsIndex(t *testing.T) {
	for in, want := range map[string]string{
		`null_resource.this[0]`:            "null_resource.this",
		`module.a.aws_s3_bucket.b["logs"]`: "module.a.aws_s3_bucket.b",
		`aws_instance.plain`:               "aws_instance.plain",
	} {
		if got := BaseAddress(in); got != want {
			t.Errorf("BaseAddress(%q) = %q, want %q", in, got, want)
		}
	}
}
