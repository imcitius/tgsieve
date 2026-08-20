package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/runner"
)

// unitSelector is one --unit: the directory someone named, and the terragrunt
// filter query it turns into.
type unitSelector struct {
	Path string // cleaned, relative to the working directory
	glob bool   // the caller wrote the pattern themselves
}

// unitConfigs are the files that make a directory a unit in its own right,
// rather than a folder with units somewhere underneath it.
var unitConfigs = []string{"terragrunt.hcl", "terragrunt.stack.hcl"}

// parseUnits turns --unit paths into filter queries. Naming a unit selects
// that unit; naming a folder above units selects everything under it, because
// someone who points at a folder means the work inside it.
func parseUnits(dir string, paths []string) ([]unitSelector, error) {
	out := make([]unitSelector, 0, len(paths))
	for _, p := range paths {
		s, err := parseUnit(dir, p)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func parseUnit(dir, raw string) (unitSelector, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return unitSelector{}, fmt.Errorf("--unit: empty path")
	}
	if filepath.IsAbs(p) {
		base, err := filepath.Abs(dir)
		if err != nil {
			return unitSelector{}, err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return unitSelector{}, fmt.Errorf("--unit %s: outside %s, which is where the run happens", raw, dir)
		}
		p = rel
	}
	p = filepath.ToSlash(filepath.Clean(p))

	// A pattern is the caller's own business: it names no single directory, so
	// there is nothing to check until something matches it.
	if hasGlob(p) {
		return unitSelector{Path: p, glob: true}, nil
	}

	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
	switch {
	case err != nil:
		return unitSelector{}, fmt.Errorf("--unit %s: no such directory under %s", raw, dir)
	case !info.IsDir():
		return unitSelector{}, fmt.Errorf("--unit %s: not a directory", raw)
	}
	return unitSelector{Path: p}, nil
}

// filterQuery is the terragrunt filter this selector becomes. A unit selects
// itself; a folder above units selects everything under it, because someone
// who points at a folder means the work inside it.
func (s unitSelector) filterQuery(dir string) string {
	switch {
	case s.glob:
		return s.Path
	case isUnitDir(filepath.Join(dir, filepath.FromSlash(s.Path))):
		return s.Path
	case s.Path == ".":
		return "**"
	}
	return s.Path + "/**"
}

func hasGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

func isUnitDir(dir string) bool {
	for _, name := range unitConfigs {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// matched reports whether any unit in the queue came from this selector.
// terragrunt says nothing about a filter that selects nothing — the queue just
// comes back shorter — so a mistyped path would run happily and report "no
// changes" for infrastructure it never looked at.
func (s unitSelector) matched(units []string) bool {
	for _, u := range units {
		u = strings.TrimPrefix(filepath.ToSlash(u), "./")
		switch {
		case s.glob:
			if matchPath(s.Path, u) {
				return true
			}
		case u == s.Path, strings.HasPrefix(u, s.Path+"/"), s.Path == ".":
			return true
		}
	}
	return false
}

// matchPath matches a unit path against a pattern the way terragrunt matches
// its filters: segment by segment, with ** standing for any depth. A pattern
// matches unit paths, not the directories above them — "envs/*" selects a unit
// called envs/prod, not the units inside it.
func matchPath(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// checkUnitsMatch names the --unit paths that select nothing, before a run that
// would otherwise do nothing and call it "no changes".
func checkUnitsMatch(dir string, sels []unitSelector, discovered []string) error {
	var missed []string
	for _, s := range sels {
		if !s.matched(discovered) {
			missed = append(missed, s.Path)
		}
	}
	if len(missed) == 0 {
		return nil
	}
	return fmt.Errorf("--unit %s matched no unit under %s (%s in the queue): check the path",
		strings.Join(missed, ", "), dir, plural(len(discovered), "unit"))
}

// selectorQueries renders the selectors as terragrunt filter queries.
func selectorQueries(dir string, sels []unitSelector) []string {
	out := make([]string, 0, len(sels))
	for _, s := range sels {
		out = append(out, s.filterQuery(dir))
	}
	return out
}

// directDirs resolves the selectors against the file system, for the engine
// that has no terragrunt to ask. Each one has to land on a root module: a
// directory with terraform files in it, which is the only thing `terraform
// plan` can be pointed at.
func directDirs(base string, sels []unitSelector) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(rel string) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		out = append(out, filepath.Join(base, filepath.FromSlash(rel)))
	}
	for _, s := range sels {
		if !s.glob {
			if !isModuleDir(filepath.Join(base, filepath.FromSlash(s.Path))) {
				return nil, fmt.Errorf("--unit %s: no terraform files there; --engine terraform runs root modules, one per --unit", s.Path)
			}
			add(s.Path)
			continue
		}
		matches, err := modulesMatching(base, s.Path)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("--unit %s matched no root module under %s: check the pattern", s.Path, base)
		}
		for _, m := range matches {
			add(m)
		}
	}
	return out, nil
}

// modulesMatching walks for the root modules a pattern selects. Terragrunt
// does this part itself; without it, this is the walk.
func modulesMatching(base, pattern string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		name := d.Name()
		if p != base && (strings.HasPrefix(name, ".") || name == "terraform.tfstate.d") {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != "." && matchPath(pattern, rel) && isModuleDir(p) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// isModuleDir reports whether terraform has anything to plan in a directory.
func isModuleDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n := e.Name(); strings.HasSuffix(n, ".tf") || strings.HasSuffix(n, ".tf.json") {
			return true
		}
	}
	return false
}

// keepSelected drops the saved plans nobody asked for, so a run over plans made
// earlier shows what it is about to apply rather than everything on disk.
func keepSelected(run *model.Run, sels []unitSelector) {
	if len(sels) == 0 {
		return
	}
	kept := make([]model.Unit, 0, len(run.Units))
	for _, u := range run.Units {
		for _, s := range sels {
			if s.matched([]string{u.Path}) {
				kept = append(kept, u)
				break
			}
		}
	}
	run.Units = kept
}

// selectedUnits parses --unit, folds the paths into the filter list, and turns
// on the stack machinery they need. A named unit is meaningless to the
// terraform engine, which drives one root module and has no queue at all.
func selectedUnits(cf commonFlags, paths []string, all *bool, filters *stringList) ([]unitSelector, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	sels, err := parseUnits(cf.dir, paths)
	if err != nil {
		return nil, err
	}
	if cf.direct() {
		// No queue to filter: the terraform engine is pointed at directories,
		// and --unit is how it gets pointed at more than one.
		return sels, nil
	}
	*all = true
	*filters = append(*filters, selectorQueries(cf.dir, sels)...)
	return sels, nil
}

// unitPaths is what the caller typed, for saying which selection came up empty.
func unitPaths(sels []unitSelector) []string {
	out := make([]string, 0, len(sels))
	for _, s := range sels {
		out = append(out, s.Path)
	}
	return out
}

// planSelection resolves --unit into the run: directories for the terraform
// engine, a filtered queue for terragrunt. Either way a selection that lands
// on nothing stops here — terragrunt says nothing about a filter that selects
// nothing, and an empty queue reports "no changes" for infrastructure the run
// never looked at.
func planSelection(ctx context.Context, opts *runner.Options, cf commonFlags, sels []unitSelector) error {
	if cf.direct() {
		dirs, err := directDirs(cf.dir, sels)
		if err != nil {
			return err
		}
		opts.Dirs = dirs
		return nil
	}
	discovered, err := runner.Discover(ctx, *opts)
	if err != nil {
		return fmt.Errorf("listing the units named by --unit: %w", err)
	}
	if err := checkUnitsMatch(cf.dir, sels, discovered); err != nil {
		return err
	}
	opts.KnownUnits = discovered
	return nil
}
