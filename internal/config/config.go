// Package config loads .tgsieve.yaml noise rules.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileNames are searched for, nearest-last, walking up from the working dir.
var FileNames = []string{".tgsieve.yaml", ".tgsieve.yml"}

type Config struct {
	Version   int               `yaml:"version"`
	Hide      Hide              `yaml:"hide"`
	Ignore    []Rule            `yaml:"ignore"`
	NeverHide NeverHide         `yaml:"never_hide"`
	Collapse  Collapse          `yaml:"collapse"`
	Normalize Normalize         `yaml:"normalize"`
	Severity  map[string]string `yaml:"severity"`

	Sources []string `yaml:"-"` // config files that produced this value
}

type Hide struct {
	// UnchangedUnits collapses units with no surviving change into a count.
	UnchangedUnits *bool `yaml:"unchanged_units"`
	// Drift hides refresh-detected drift (shown in its own section otherwise).
	Drift *bool `yaml:"drift"`
	// Outputs hides output-only changes.
	Outputs *bool `yaml:"outputs"`
}

// Rule removes attributes from resources it matches. Every selector that is
// set must match. Attrs is required: use ["*"] to drop the whole resource.
type Rule struct {
	Name    string   `yaml:"name"`
	Unit    string   `yaml:"unit"`
	Type    string   `yaml:"type"`
	Address string   `yaml:"address"`
	Actions []string `yaml:"actions"`
	Attrs   []string `yaml:"attrs"`

	unitRe, typeRe, addrRe *regexp.Regexp
	attrRes                []*regexp.Regexp
}

// NeverHide is the safety net: matching resources bypass every ignore rule.
type NeverHide struct {
	Actions []string `yaml:"actions"`
	Types   []string `yaml:"types"`

	typeRes []*regexp.Regexp
}

// Normalize decides which differences are treated as no difference at all.
// These are judgement calls about terraform's own representation, so they are
// config rather than hardcoded: a plan that hides something must be able to
// say which rule hid it.
type Normalize struct {
	// EmptyAsNull treats "", [], {} and null as the same value.
	EmptyAsNull *bool `yaml:"empty_as_null"`
	// Reorder decides what to do when a collection comes back in a different
	// order with the same members: "show" (one line) or "ignore".
	Reorder string `yaml:"reorder"`
}

type Collapse struct {
	Instances     *bool  `yaml:"instances"`
	CrossUnit     *bool  `yaml:"cross_unit"`
	CrossUnitMode string `yaml:"cross_unit_mode"` // "shape" (default) or "strict"
	MinUnits      int    `yaml:"min_units"`
}

func Default() *Config {
	t, f := true, false
	return &Config{
		Version:   1,
		Hide:      Hide{UnchangedUnits: &t, Drift: &f, Outputs: &f},
		NeverHide: NeverHide{Actions: []string{"delete", "replace"}},
		Collapse:  Collapse{Instances: &t, CrossUnit: &t, CrossUnitMode: "shape", MinUnits: 2},
		// Nothing is normalized away by default; both of these are opinions
		// about someone else's infrastructure.
		Normalize: Normalize{EmptyAsNull: &f, Reorder: "show"},
	}
}

// Load walks up from dir collecting config files (root first, nearest last) and
// merges them. An explicit path short-circuits the search.
func Load(dir, explicit string) (*Config, error) {
	cfg := Default()
	if explicit != "" {
		c, err := parseFile(explicit)
		if err != nil {
			return nil, err
		}
		merge(cfg, c)
		cfg.Sources = append(cfg.Sources, explicit)
		return cfg, cfg.compile()
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var found []string
	for d := abs; ; {
		for _, name := range FileNames {
			p := filepath.Join(d, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				found = append(found, p)
				break
			}
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			break // stop at repo root
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	// found is nearest-first; apply root-first so nearer files win.
	for i := len(found) - 1; i >= 0; i-- {
		c, err := parseFile(found[i])
		if err != nil {
			return nil, err
		}
		merge(cfg, c)
		cfg.Sources = append(cfg.Sources, found[i])
	}
	return cfg, cfg.compile()
}

// RootMarkers identify the top of a terragrunt project when there is no git
// repository to anchor on.
var RootMarkers = []string{"root.hcl", "terragrunt.hcl", "terragrunt.stack.hcl"}

// ProjectRoot walks up from dir looking for the top of the project: a git
// repository root, or failing that the highest directory that still carries a
// terragrunt root config. Falls back to dir itself.
func ProjectRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	highestMarker := ""
	for d := abs; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		for _, m := range RootMarkers {
			if _, err := os.Stat(filepath.Join(d, m)); err == nil {
				highestMarker = d
				break
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if highestMarker != "" {
		return highestMarker, nil
	}
	return abs, nil
}

func parseFile(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func merge(dst, src *Config) {
	if src == nil {
		return
	}
	if src.Version != 0 {
		dst.Version = src.Version
	}
	if src.Hide.UnchangedUnits != nil {
		dst.Hide.UnchangedUnits = src.Hide.UnchangedUnits
	}
	if src.Hide.Drift != nil {
		dst.Hide.Drift = src.Hide.Drift
	}
	if src.Hide.Outputs != nil {
		dst.Hide.Outputs = src.Hide.Outputs
	}
	dst.Ignore = append(dst.Ignore, src.Ignore...)
	if len(src.NeverHide.Actions) > 0 {
		dst.NeverHide.Actions = src.NeverHide.Actions
	}
	if len(src.NeverHide.Types) > 0 {
		dst.NeverHide.Types = append(dst.NeverHide.Types, src.NeverHide.Types...)
	}
	if src.Collapse.Instances != nil {
		dst.Collapse.Instances = src.Collapse.Instances
	}
	if src.Collapse.CrossUnit != nil {
		dst.Collapse.CrossUnit = src.Collapse.CrossUnit
	}
	if src.Collapse.CrossUnitMode != "" {
		dst.Collapse.CrossUnitMode = src.Collapse.CrossUnitMode
	}
	if src.Collapse.MinUnits != 0 {
		dst.Collapse.MinUnits = src.Collapse.MinUnits
	}
	if src.Normalize.EmptyAsNull != nil {
		dst.Normalize.EmptyAsNull = src.Normalize.EmptyAsNull
	}
	if src.Normalize.Reorder != "" {
		dst.Normalize.Reorder = src.Normalize.Reorder
	}
	if len(src.Severity) > 0 {
		if dst.Severity == nil {
			dst.Severity = map[string]string{}
		}
		for k, v := range src.Severity {
			dst.Severity[k] = v
		}
	}
}

// Compile validates a Config built in memory (tests, or callers that assemble
// rules programmatically) and prepares its glob matchers.
func Compile(c *Config) error { return c.compile() }

func (c *Config) compile() error {
	for i := range c.Ignore {
		r := &c.Ignore[i]
		if len(r.Attrs) == 0 {
			return fmt.Errorf("ignore rule %q: 'attrs' is required (use attrs: [\"*\"] to drop the whole resource)", r.Label(i))
		}
		var err error
		if r.unitRe, err = compileGlob(r.Unit); err != nil {
			return fmt.Errorf("ignore rule %q unit: %w", r.Label(i), err)
		}
		if r.typeRe, err = compileGlob(r.Type); err != nil {
			return fmt.Errorf("ignore rule %q type: %w", r.Label(i), err)
		}
		if r.addrRe, err = compileGlob(r.Address); err != nil {
			return fmt.Errorf("ignore rule %q address: %w", r.Label(i), err)
		}
		r.attrRes = r.attrRes[:0]
		for _, a := range r.Attrs {
			re, err := compileGlob(a)
			if err != nil {
				return fmt.Errorf("ignore rule %q attr %q: %w", r.Label(i), a, err)
			}
			r.attrRes = append(r.attrRes, re)
		}
	}
	c.NeverHide.typeRes = nil
	for _, t := range c.NeverHide.Types {
		re, err := compileGlob(t)
		if err != nil {
			return fmt.Errorf("never_hide type %q: %w", t, err)
		}
		c.NeverHide.typeRes = append(c.NeverHide.typeRes, re)
	}
	switch c.Collapse.CrossUnitMode {
	case "", "shape", "strict":
	default:
		return fmt.Errorf("collapse.cross_unit_mode: want \"shape\" or \"strict\", got %q", c.Collapse.CrossUnitMode)
	}
	switch c.Normalize.Reorder {
	case "", "show", "ignore":
	default:
		return fmt.Errorf("normalize.reorder: want \"show\" or \"ignore\", got %q", c.Normalize.Reorder)
	}
	for k, v := range c.Severity {
		switch v {
		case "low", "medium", "high":
		default:
			return fmt.Errorf("severity.%s: want low|medium|high, got %q", k, v)
		}
	}
	return nil
}

// Label names a rule for --explain output.
func (r Rule) Label(i int) string {
	if r.Name != "" {
		return r.Name
	}
	parts := []string{}
	if r.Type != "" {
		parts = append(parts, "type="+r.Type)
	}
	if r.Unit != "" {
		parts = append(parts, "unit="+r.Unit)
	}
	if r.Address != "" {
		parts = append(parts, "address="+r.Address)
	}
	parts = append(parts, "attrs="+strings.Join(r.Attrs, ","))
	return fmt.Sprintf("#%d %s", i+1, strings.Join(parts, " "))
}

// MatchesResource reports whether the rule's selectors match a resource.
func (r Rule) MatchesResource(unit, typ, address string, action string) bool {
	if r.unitRe != nil && !r.unitRe.MatchString(unit) {
		return false
	}
	if r.typeRe != nil && !r.typeRe.MatchString(typ) {
		return false
	}
	if r.addrRe != nil && !r.addrRe.MatchString(address) {
		return false
	}
	if len(r.Actions) > 0 {
		ok := false
		for _, a := range r.Actions {
			if a == action {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// MatchesAttr reports whether an attribute path is covered by the rule.
func (r Rule) MatchesAttr(path string) bool {
	for _, re := range r.attrRes {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

func (n NeverHide) Matches(action, typ string) bool {
	for _, a := range n.Actions {
		if a == action {
			return true
		}
	}
	for _, re := range n.typeRes {
		if re.MatchString(typ) {
			return true
		}
	}
	return false
}

// compileGlob turns a glob into an anchored regexp.
//
//   - matches anything except a path separator '/'  (so it crosses '.')
//     ** matches anything at all
//     ?  matches one character that is not '/'
func compileGlob(g string) (*regexp.Regexp, error) {
	if g == "" {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch g[i] {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
