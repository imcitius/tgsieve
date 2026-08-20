package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imcitius/tgsieve/internal/model"
)

// stack builds a small tree: two units, and a folder that only holds units.
func stack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, u := range []string{"envs/prod/eks", "envs/stage/eks"} {
		dir := filepath.Join(root, filepath.FromSlash(u))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "terragrunt.hcl"), []byte("\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestParseUnitNamesTheUnitItself(t *testing.T) {
	root := stack(t)
	got, err := parseUnit(root, "envs/prod/eks")
	if err != nil {
		t.Fatal(err)
	}
	if got.Query != "envs/prod/eks" {
		t.Errorf("query = %q, want the unit path", got.Query)
	}
}

func TestParseUnitTakesEverythingUnderAFolder(t *testing.T) {
	// Someone naming a folder means the work inside it; terragrunt matches
	// unit paths exactly, so the folder has to become a pattern.
	root := stack(t)
	got, err := parseUnit(root, "envs/prod")
	if err != nil {
		t.Fatal(err)
	}
	if got.Query != "envs/prod/**" {
		t.Errorf("query = %q, want envs/prod/**", got.Query)
	}
}

func TestParseUnitTrimsAndAcceptsAbsolutePaths(t *testing.T) {
	root := stack(t)
	got, err := parseUnit(root, filepath.Join(root, "envs/prod/eks")+"/")
	if err != nil {
		t.Fatal(err)
	}
	if got.Query != "envs/prod/eks" {
		t.Errorf("query = %q, want it relative to the working directory", got.Query)
	}
}

func TestParseUnitRefusesWhatCannotBeRun(t *testing.T) {
	root := stack(t)
	if _, err := parseUnit(root, "envs/prd/eks"); err == nil {
		t.Error("a typo should be reported, not planned as an empty queue")
	}
	if _, err := parseUnit(root, filepath.Join(root, "..", "elsewhere")); err == nil {
		t.Error("a path outside the working directory cannot be part of the run")
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseUnit(root, "notes.md"); err == nil {
		t.Error("a file is not a unit")
	}
}

func TestParseUnitPassesPatternsThrough(t *testing.T) {
	root := stack(t)
	got, err := parseUnit(root, "envs/*/eks")
	if err != nil {
		t.Fatal("a pattern names no directory of its own, so it cannot be checked for existence")
	}
	if got.Query != "envs/*/eks" || !got.glob {
		t.Errorf("pattern = %+v, want it handed to terragrunt as written", got)
	}
}

func TestSelectorMatching(t *testing.T) {
	queue := []string{"envs/prod/eks", "envs/prod/vpc", "envs/stage/eks"}
	cases := []struct {
		sel  unitSelector
		want bool
	}{
		{unitSelector{Path: "envs/prod/eks"}, true},
		{unitSelector{Path: "envs/prod"}, true}, // the folder above two units
		{unitSelector{Path: "envs/dev"}, false}, // nothing under it
		{unitSelector{Path: "envs/prod/ek"}, false},
		{unitSelector{Path: "envs/*/eks", glob: true}, true},
		{unitSelector{Path: "envs/nope*", glob: true}, false},
		{unitSelector{Path: "envs/**", glob: true}, true},
		// terragrunt matches unit paths, not the directories above them.
		{unitSelector{Path: "envs/*", glob: true}, false},
	}
	for _, c := range cases {
		if got := c.sel.matched(queue); got != c.want {
			t.Errorf("%q matched = %v, want %v", c.sel.Path, got, c.want)
		}
	}
}

func TestCheckUnitsMatchNamesWhatSelectedNothing(t *testing.T) {
	sels := []unitSelector{{Path: "envs/prod/eks"}, {Path: "envs/dev/eks"}}
	err := checkUnitsMatch(".", sels, []string{"envs/prod/eks"})
	if err == nil {
		t.Fatal("a --unit that selects nothing must stop the run, not shorten it")
	}
	if !strings.Contains(err.Error(), "envs/dev/eks") {
		t.Errorf("error should name the path that missed: %v", err)
	}
	if strings.Contains(err.Error(), "envs/prod/eks") {
		t.Errorf("the path that matched is not the news: %v", err)
	}
	if err := checkUnitsMatch(".", sels[:1], []string{"envs/prod/eks"}); err != nil {
		t.Errorf("everything matched: %v", err)
	}
}

func TestKeepSelectedDropsPlansNobodyAskedFor(t *testing.T) {
	// What the report shows and what the apply runs have to be the same set.
	run := model.Run{Units: []model.Unit{
		{Path: "envs/prod/eks"}, {Path: "envs/prod/vpc"}, {Path: "envs/stage/eks"},
	}}
	keepSelected(&run, []unitSelector{{Path: "envs/prod/vpc"}})
	if len(run.Units) != 1 || run.Units[0].Path != "envs/prod/vpc" {
		t.Fatalf("units = %+v, want only the selected one", run.Units)
	}

	all := model.Run{Units: []model.Unit{{Path: "envs/prod/eks"}}}
	keepSelected(&all, nil)
	if len(all.Units) != 1 {
		t.Error("no selection means no filtering")
	}
}
