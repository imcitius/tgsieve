package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGlobSemantics(t *testing.T) {
	cases := []struct {
		glob, in string
		want     bool
	}{
		{"tags.*", "tags.env", true},
		{"tags.*", "tags.a.b", true},   // * crosses dots
		{"*", "anything.at.all", true}, // "*" is the whole-resource escape hatch
		{"aws_ecs_*", "aws_ecs_service", true},
		{"aws_ecs_*", "aws_s3_bucket", false},
		{"envs/dev/**", "envs/dev/eu/app", true},
		{"envs/dev/*", "envs/dev/eu/app", false}, // * stops at '/'
		{"envs/*/app", "envs/prod/app", true},
	}
	for _, c := range cases {
		re, err := compileGlob(c.glob)
		if err != nil {
			t.Fatalf("compileGlob(%q): %v", c.glob, err)
		}
		if got := re.MatchString(c.in); got != c.want {
			t.Errorf("glob %q vs %q = %v, want %v", c.glob, c.in, got, c.want)
		}
	}
}

func TestRuleRequiresAttrs(t *testing.T) {
	c := Default()
	c.Ignore = []Rule{{Name: "oops", Type: "aws_x"}}
	if err := Compile(c); err == nil {
		t.Fatal("a rule without attrs should be rejected")
	}
}

func TestLoadWalksUpAndNearerWins(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "envs", "prod")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(dir, body string) {
		if err := os.WriteFile(filepath.Join(dir, ".tgsieve.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(root, "version: 1\nignore:\n  - name: root rule\n    attrs: [\"tags.*\"]\ncollapse:\n  min_units: 5\n")
	write(deep, "version: 1\nignore:\n  - name: leaf rule\n    attrs: [\"revision\"]\ncollapse:\n  min_units: 9\n")

	cfg, err := Load(deep, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Ignore) != 2 {
		t.Fatalf("rules should accumulate, got %d", len(cfg.Ignore))
	}
	if cfg.Ignore[0].Name != "root rule" || cfg.Ignore[1].Name != "leaf rule" {
		t.Errorf("order = %q, %q", cfg.Ignore[0].Name, cfg.Ignore[1].Name)
	}
	if cfg.Collapse.MinUnits != 9 {
		t.Errorf("nearer config should win for scalars: MinUnits = %d", cfg.Collapse.MinUnits)
	}
	if len(cfg.Sources) != 2 {
		t.Errorf("Sources = %v", cfg.Sources)
	}
}

func TestUnknownFieldIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".tgsieve.yaml"), []byte("ignroe: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, filepath.Join(dir, ".tgsieve.yaml")); err == nil {
		t.Fatal("a typo in the config should not be silently ignored")
	}
}
