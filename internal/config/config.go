// Package config loads .tgsieve.yaml noise rules.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/imcitius/tgsieve/internal/attrpath"
)

// FileNames are searched for, nearest-last, walking up from the working dir.
var FileNames = []string{".tgsieve.yaml", ".tgsieve.yml"}

type Config struct {
	Version   int               `yaml:"version"`
	Extends   []string          `yaml:"extends"`
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
	// Reads hides data sources that will be read during apply. They create
	// nothing, so they are hidden by default; set false to see them when
	// working out why a value is unknown.
	Reads *bool `yaml:"reads"`
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
	// Expires ends a suppression on a date (YYYY-MM-DD). Past it the rule
	// stops hiding and says so, so "just until we fix it" cannot quietly
	// become permanent blindness.
	Expires string `yaml:"expires"`

	expiresAt              time.Time
	unitRe, typeRe, addrRe *regexp.Regexp
	attrRes                []*regexp.Regexp
}

// ExpiryDate is the format accepted for rule expiry.
const ExpiryDate = "2006-01-02"

// Expired reports whether the rule has lapsed, and when it did.
func (r Rule) Expired(now time.Time) bool {
	return !r.expiresAt.IsZero() && now.After(r.expiresAt)
}

// ExpiresAt returns the parsed expiry, zero when the rule has none.
func (r Rule) ExpiresAt() time.Time { return r.expiresAt }

// NeverHide is the safety net: matching resources bypass every ignore rule.
//
// Actions is a pointer so that an explicit empty list can turn the net off.
// Ignoring what someone wrote because it was empty would be the worst of both
// worlds: the setting looks applied and is not.
type NeverHide struct {
	Actions *[]string `yaml:"actions"`
	Types   []string  `yaml:"types"`

	typeRes []*regexp.Regexp
}

// List returns the configured actions, for display.
func (n NeverHide) List() []string { return n.actions() }

func (n NeverHide) actions() []string {
	if n.Actions == nil {
		return nil
	}
	return *n.Actions
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

// DefaultSeverity ranks the actions when nothing is configured. Anything that
// removes or recreates infrastructure is high; changing one in place is
// medium; adding something new is low.
var DefaultSeverity = map[string]string{
	"delete":  "high",
	"replace": "high",
	"update":  "medium",
	"create":  "low",
}

// SeverityOf ranks one action, honouring any override in the config.
func (c *Config) SeverityOf(action string) string {
	if v, ok := c.Severity[action]; ok {
		return v
	}
	if v, ok := DefaultSeverity[action]; ok {
		return v
	}
	return "low"
}

// SeverityRank orders the levels; an unknown level sorts lowest.
func SeverityRank(level string) int {
	switch level {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

func Default() *Config {
	t, f := true, false
	return &Config{
		Version:   1,
		Hide:      Hide{UnchangedUnits: &t, Drift: &f, Outputs: &f, Reads: &t},
		NeverHide: NeverHide{Actions: &[]string{"delete", "replace"}},
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
	if err := yamlStrict(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// yamlStrict decodes and rejects unknown fields, so a typo in a config is an
// error rather than a silently ignored setting.
func yamlStrict(b []byte, out *Config) error {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	return dec.Decode(out)
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
	if src.Hide.Reads != nil {
		dst.Hide.Reads = src.Hide.Reads
	}
	dst.Extends = append(dst.Extends, src.Extends...)
	dst.Ignore = append(dst.Ignore, src.Ignore...)
	if src.NeverHide.Actions != nil {
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
	// Presets come first so a rule written by hand can be read as the last
	// word, and so --explain attributes each suppression to its source.
	if len(c.Extends) > 0 {
		var expanded []Rule
		seen := map[string]bool{}
		for _, name := range c.Extends {
			if seen[name] {
				continue
			}
			seen[name] = true
			rules, err := LoadPreset(name)
			if err != nil {
				return err
			}
			expanded = append(expanded, rules...)
		}
		c.Ignore = append(expanded, c.Ignore...)
		c.Extends = nil // expanded once; compile() may run again
	}
	for i := range c.Ignore {
		r := &c.Ignore[i]
		if len(r.Attrs) == 0 {
			return fmt.Errorf("ignore rule %q: 'attrs' is required (use attrs: [\"*\"] to drop the whole resource)", r.Label(i))
		}
		if r.Expires != "" {
			t, err := time.Parse(ExpiryDate, r.Expires)
			if err != nil {
				return fmt.Errorf("ignore rule %q expires: want a date like 2026-12-01, got %q", r.Label(i), r.Expires)
			}
			// A rule expires at the end of the day it names.
			r.expiresAt = t.Add(24*time.Hour - time.Nanosecond)
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
			re, err := compileAttrGlob(a)
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
//
// A path whose key had to be quoted is also matched in its plain dotted form,
// so a rule written as labels.* covers labels["app.kubernetes.io/name"] too —
// one syntax for the reader, both spellings underneath.
func (r Rule) MatchesAttr(path string) bool {
	dotted := attrpath.Dotted(path)
	for _, re := range r.attrRes {
		if re.MatchString(path) || (dotted != path && re.MatchString(dotted)) {
			return true
		}
	}
	return false
}

func (n NeverHide) Matches(action, typ string) bool {
	for _, a := range n.actions() {
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

// compileGlob turns a glob over unit paths, types and addresses into an
// anchored regexp:
//
//   - matches anything except a path separator '/'  (so it crosses '.')
//     ** matches anything at all
//     ?  matches one character that is not '/'
func compileGlob(g string) (*regexp.Regexp, error) { return buildGlob(g, true) }

// compileAttrGlob is the same for attribute patterns, where '/' carries no
// structure — it appears inside keys such as "app.kubernetes.io/name" — so a
// single '*' matches through it.
func compileAttrGlob(g string) (*regexp.Regexp, error) { return buildGlob(g, false) }

func buildGlob(g string, slashIsSeparator bool) (*regexp.Regexp, error) {
	if g == "" {
		return nil, nil
	}
	star, one := ".*", "."
	if slashIsSeparator {
		star, one = "[^/]*", "[^/]"
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
				b.WriteString(star)
			}
		case '?':
			b.WriteString(one)
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
