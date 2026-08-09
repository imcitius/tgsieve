package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
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
