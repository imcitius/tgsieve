package tfplan

import (
	"encoding/json"
	"strings"
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

func TestReorderedSetIsOneFactNotMany(t *testing.T) {
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"cidrs": ["10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24", "10.0.4.0/24"]},
	  "after":  {"cidrs": ["10.0.4.0/24", "10.0.1.0/24", "10.0.3.0/24", "10.0.2.0/24"]}
	}`)

	attrs := Diff(ch)
	if len(attrs) != 1 {
		t.Fatalf("a reordering is one fact, got %d entries: %+v", len(attrs), attrs)
	}
	if attrs[0].Kind != model.KindReordered || attrs[0].Path != "cidrs" || attrs[0].Count != 4 {
		t.Errorf("got %+v", attrs[0])
	}
}

func TestSetMembershipReportedAsAddedAndRemoved(t *testing.T) {
	// One rule removed from the middle: positionally everything after it
	// shifts, which used to read as four separate changes.
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"ports": [22, 80, 443, 8080, 9090]},
	  "after":  {"ports": [22, 443, 8080, 9090]}
	}`)

	attrs := Diff(ch)
	if len(attrs) != 1 {
		t.Fatalf("want a single removal, got %d: %+v", len(attrs), attrs)
	}
	if attrs[0].Kind != model.KindRemoved || attrs[0].Before != float64(80) {
		t.Errorf("got %+v", attrs[0])
	}
}

func TestSingleElementEditStaysPositional(t *testing.T) {
	// Nothing moved, so the index is the clearest way to say it.
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"ports": [22, 80, 443]},
	  "after":  {"ports": [22, 81, 443]}
	}`)

	attrs := Diff(ch)
	if len(attrs) != 1 {
		t.Fatalf("want one change, got %d: %+v", len(attrs), attrs)
	}
	if attrs[0].Path != "ports.1" || attrs[0].Kind != model.KindChanged {
		t.Errorf("got %+v", attrs[0])
	}
}

func TestMapKeysWithDotsAreQuoted(t *testing.T) {
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"labels": {"app.kubernetes.io/name": "old", "plain": "same"}},
	  "after":  {"labels": {"app.kubernetes.io/name": "new", "plain": "same"}}
	}`)

	attrs := Diff(ch)
	if len(attrs) != 1 {
		t.Fatalf("want one change, got %d: %+v", len(attrs), attrs)
	}
	want := `labels["app.kubernetes.io/name"]`
	if attrs[0].Path != want {
		t.Errorf("path = %q, want %q", attrs[0].Path, want)
	}
	if segs := SplitPath(attrs[0].Path); len(segs) != 2 || segs[1] != "app.kubernetes.io/name" {
		t.Errorf("path does not split back into its segments: %#v", segs)
	}
}

func TestUnknownUnderQuotedKey(t *testing.T) {
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"labels": {"a.b": "old"}},
	  "after":  {"labels": {"a.b": null}},
	  "after_unknown": {"labels": {"a.b": true}}
	}`)

	attrs := Diff(ch)
	if len(attrs) != 1 || !attrs[0].AfterUnknown {
		t.Fatalf("unknown mask must line up with the quoted path: %+v", attrs)
	}
	if attrs[0].Path != `labels["a.b"]` {
		t.Errorf("path = %q", attrs[0].Path)
	}
}

func TestShrinkingListReportsMembershipNotShiftedIndexes(t *testing.T) {
	// Dropping an element from the middle shifts every later index; reporting
	// positions would claim changes that never happened, ending in a
	// "value -> null" for an element that only moved up.
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"cidrs": ["10.0.4.0/24", "10.0.1.0/24", "10.0.3.0/24", "10.0.2.0/24"]},
	  "after":  {"cidrs": ["10.0.4.0/24", "10.0.1.0/24", "10.0.9.0/24"]}
	}`)

	attrs := Diff(ch)
	kinds := map[string]int{}
	for _, a := range attrs {
		if a.Path != "cidrs" {
			t.Errorf("membership changes belong to the attribute, not an index: %q", a.Path)
		}
		kinds[a.Kind]++
	}
	if kinds[model.KindRemoved] != 2 || kinds[model.KindAdded] != 1 {
		t.Errorf("want 2 removed and 1 added, got %v (%+v)", kinds, attrs)
	}
}

func TestAppendingToAListStaysPositional(t *testing.T) {
	// Nothing shifted, so index 2 is exactly where the new element is.
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"zones": ["a", "b"]},
	  "after":  {"zones": ["a", "b", "c"]}
	}`)

	attrs := Diff(ch)
	if len(attrs) != 1 || attrs[0].Path != "zones.2" {
		t.Fatalf("want a single positional addition, got %+v", attrs)
	}
}

func TestObjectsInACollectionArePairedByIdentity(t *testing.T) {
	// One rule's port range widened. Reporting that as "this object left, this
	// other object arrived" makes the reader diff two JSON blobs by eye.
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"ingress": [
	    {"name": "web",  "from_port": 80,  "to_port": 80},
	    {"name": "ssh",  "from_port": 22,  "to_port": 22}
	  ]},
	  "after": {"ingress": [
	    {"name": "ssh",  "from_port": 22,  "to_port": 22},
	    {"name": "web",  "from_port": 80,  "to_port": 8080}
	  ]}
	}`)

	attrs := Diff(ch)
	if len(attrs) != 1 {
		t.Fatalf("want the single edited field, got %d: %+v", len(attrs), attrs)
	}
	if got, want := attrs[0].Path, `ingress["web"].to_port`; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if attrs[0].Before != float64(80) || attrs[0].After != float64(8080) {
		t.Errorf("values = %v -> %v", attrs[0].Before, attrs[0].After)
	}
}

func TestUnpairableObjectsStayAsMembership(t *testing.T) {
	// Nothing identifies these, so the honest report is what left and what
	// arrived.
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"rules": [{"from": 1, "to": 2}, {"from": 3, "to": 4}]},
	  "after":  {"rules": [{"from": 3, "to": 4}, {"from": 5, "to": 6}]}
	}`)

	attrs := Diff(ch)
	kinds := map[string]int{}
	for _, a := range attrs {
		kinds[a.Kind]++
	}
	if kinds[model.KindRemoved] != 1 || kinds[model.KindAdded] != 1 {
		t.Errorf("want one removal and one addition, got %v (%+v)", kinds, attrs)
	}
}

func TestIdentityMustBeUniqueToBeUsed(t *testing.T) {
	// A repeated "name" is a label, not an identity; pairing on it would
	// invent a correspondence that does not exist. Positions are then the
	// honest reading, and they are what should appear.
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"rules": [{"name": "x", "port": 1}, {"name": "x", "port": 2}]},
	  "after":  {"rules": [{"name": "x", "port": 3}, {"name": "x", "port": 4}]}
	}`)

	for _, a := range Diff(ch) {
		if strings.Contains(a.Path, `["x"]`) {
			t.Errorf("paired on a name that identifies nothing: %+v", a)
		}
	}
}

func TestPairedObjectAndLeftoverTogether(t *testing.T) {
	ch := changeFrom(t, `{
	  "actions": ["update"],
	  "before": {"ingress": [{"name": "web", "port": 80}, {"name": "old", "port": 21}]},
	  "after":  {"ingress": [{"name": "web", "port": 8080}, {"name": "new", "port": 22}]}
	}`)

	var edited, removed, added int
	for _, a := range Diff(ch) {
		switch a.Kind {
		case model.KindRemoved:
			removed++
		case model.KindAdded:
			added++
		default:
			edited++
			if a.Path != `ingress["web"].port` {
				t.Errorf("unexpected edited path %q", a.Path)
			}
		}
	}
	if edited != 1 || removed != 1 || added != 1 {
		t.Errorf("want one edit, one removal and one addition; got %d/%d/%d", edited, removed, added)
	}
}
