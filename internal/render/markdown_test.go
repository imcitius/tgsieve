package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/sieve"
)

func group(action model.Action, unit, addr string, attrs ...model.AttrChange) sieve.Group {
	r := model.Resource{
		Unit: unit, Address: addr, BaseAddress: addr, Action: action, Attrs: attrs,
		Type: "aws_thing", Name: "x",
	}
	return sieve.Group{Action: action, Sample: r, Members: []model.Resource{r}, Units: []string{unit}}
}

func renderMD(t *testing.T, rep *sieve.Report, opts Options) string {
	t.Helper()
	var buf bytes.Buffer
	Markdown(&buf, rep, opts)
	return buf.String()
}

func TestMarkdownKeepsDestructiveChangesUnfolded(t *testing.T) {
	rep := &sieve.Report{
		Kept:         model.Counts{Delete: 1, Update: 1},
		UnitsTotal:   2,
		UnitsChanged: 2,
		Groups: []sieve.Group{
			group(model.ActionDelete, "envs/prod/db", "aws_db_instance.main"),
			group(model.ActionUpdate, "envs/prod/web", "aws_ecs_service.app",
				model.AttrChange{Path: "desired_count", Before: 2.0, After: 4.0}),
		},
	}

	got := renderMD(t, rep, Options{})
	destroyAt := strings.Index(got, "### Destroy / replace")
	if destroyAt < 0 {
		t.Fatalf("no destroy section:\n%s", got)
	}
	if strings.Contains(got[:destroyAt], "<details>") {
		t.Error("a destroy must not be hidden behind a fold")
	}
	if !strings.Contains(got, "<details><summary><b>Update (1)</b>") {
		t.Errorf("updates should fold away:\n%s", got)
	}
	if !strings.Contains(got, "**1 resource will be destroyed or replaced.**") {
		t.Errorf("the warning line is missing:\n%s", got)
	}
}

func TestMarkdownTrimsInsteadOfOverflowing(t *testing.T) {
	var groups []sieve.Group
	for i := 0; i < 200; i++ {
		groups = append(groups, group(model.ActionUpdate, "envs/prod/unit", "aws_thing.number",
			model.AttrChange{Path: "some_attribute_with_a_long_name", Before: strings.Repeat("a", 60), After: strings.Repeat("b", 60)}))
	}
	rep := &sieve.Report{Kept: model.Counts{Update: 200}, UnitsTotal: 1, UnitsChanged: 1, Groups: groups}

	got := renderMD(t, rep, Options{MaxBytes: 2000})
	if len(got) > 2400 {
		t.Errorf("output ran past its budget: %d bytes", len(got))
	}
	if !strings.Contains(got, "Output trimmed") {
		t.Errorf("a trimmed report must say so:\n%s", got[len(got)-200:])
	}
}

func TestMarkdownSaysWhenThereIsNothingToDo(t *testing.T) {
	got := renderMD(t, &sieve.Report{UnitsTotal: 12}, Options{})
	if !strings.Contains(got, "## tgsieve — no changes") {
		t.Errorf("headline should carry the answer:\n%s", got)
	}
}

func TestMarkdownEscapesValuesThatWouldBreakCodeSpans(t *testing.T) {
	rep := &sieve.Report{
		Kept: model.Counts{Update: 1}, UnitsTotal: 1, UnitsChanged: 1,
		Groups: []sieve.Group{group(model.ActionUpdate, "u", "aws_thing.x",
			model.AttrChange{Path: "policy", Before: "a`b", After: "c`d"})},
	}
	got := renderMD(t, rep, Options{})
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "policy") && strings.Count(line, "`")%2 != 0 {
			t.Errorf("unbalanced code span: %q", line)
		}
	}
}

func TestMarkdownReportsFailuresFirst(t *testing.T) {
	rep := &sieve.Report{
		UnitsTotal:   3,
		ErroredUnits: []model.Unit{{Path: "a", Errored: true, Error: "Error: boom"}},
		Failures: []sieve.FailureGroup{{
			Headline: "Error: boom", Units: []string{"a"}, Detail: []string{"Error: boom"},
		}},
	}
	got := renderMD(t, rep, Options{})
	if !strings.HasPrefix(got, Marker+"\n## tgsieve — failed") {
		t.Errorf("the marker should lead, then the headline:\n%s", got)
	}
	if !strings.Contains(got, "### Failed (1)") {
		t.Errorf("missing failure section:\n%s", got)
	}
}

func TestMarkdownFilesReadsSeparatelyFromCreates(t *testing.T) {
	read := group(model.ActionRead, "u", "data.aws_ami.this")
	read.Sample.Mode = "data"
	rep := &sieve.Report{
		Kept: model.Counts{Create: 1}, UnitsTotal: 1, UnitsChanged: 1,
		Groups: []sieve.Group{group(model.ActionCreate, "u", "aws_instance.web"), read},
	}

	got := renderMD(t, rep, Options{})
	if !strings.Contains(got, "Create (1)") {
		t.Errorf("the create section should count only the creation:\n%s", got)
	}
	if !strings.Contains(got, "Read (data sources, resolved during apply) (1)") {
		t.Errorf("reads deserve their own section:\n%s", got)
	}
}

func TestValuesAreTrimmedAroundTheDifference(t *testing.T) {
	// The values start out identical, so cutting at a fixed offset shows the
	// part that did not change and hides the part that did.
	before := strings.Repeat("x", 200) + "GetObject" + strings.Repeat("y", 200)
	after := strings.Repeat("x", 200) + "DeleteObject" + strings.Repeat("y", 200)

	b, a := diffView(before, after, 60)
	if !strings.Contains(b, "GetObject") {
		t.Errorf("the difference is missing from the old value: %q", b)
	}
	if !strings.Contains(a, "DeleteObject") {
		t.Errorf("the difference is missing from the new value: %q", a)
	}
	if len([]rune(b)) > 62 || len([]rune(a)) > 62 {
		t.Errorf("the budget was ignored: %d and %d runes", len([]rune(b)), len([]rune(a)))
	}
}

func TestNoLimitShowsEverything(t *testing.T) {
	long := strings.Repeat("a", 500)
	b, a := diffView(long+"1", long+"2", 0)
	if len(b) != 501 || len(a) != 501 {
		t.Errorf("--max-value 0 should not trim: %d, %d", len(b), len(a))
	}
}

func TestEmbeddedJSONIsUnwrapped(t *testing.T) {
	// A policy document is JSON inside a string; quoting it again escapes
	// every quote it contains.
	got := fmtVal(`{"Version":"2012-10-17","Statement":[]}`, 0)
	if strings.Contains(got, `\"`) {
		t.Errorf("escaped quotes survived: %s", got)
	}
	if !strings.HasPrefix(got, "{") {
		t.Errorf("not unwrapped: %s", got)
	}
	// Ordinary strings keep their quotes, so a value is never confused with a
	// bare word.
	if got := fmtVal("plain", 0); got != `"plain"` {
		t.Errorf("fmtVal(plain) = %s", got)
	}
}
