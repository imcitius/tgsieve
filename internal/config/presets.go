package config

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed presets/*.yaml
var presetFS embed.FS

// Presets are curated rule sets shipped with tgsieve, so the same handful of
// suppressions is not rewritten in every repository. They are opt-in: a tool
// whose job is to hide things must never decide on its own what to hide.
//
//	extends: [builtin/aws-tags]
const presetPrefix = "builtin/"

// PresetNames lists the available presets.
func PresetNames() []string {
	entries, err := fs.ReadDir(presetFS, "presets")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, presetPrefix+strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(names)
	return names
}

// LoadPreset returns the rules of one preset, each labelled with its origin so
// --explain can say where a suppression came from.
func LoadPreset(name string) ([]Rule, error) {
	short := strings.TrimPrefix(name, presetPrefix)
	if short == name {
		return nil, fmt.Errorf("unknown preset %q: names look like %s", name, strings.Join(PresetNames(), ", "))
	}
	b, err := presetFS.ReadFile("presets/" + short + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("unknown preset %q: available presets are %s", name, strings.Join(PresetNames(), ", "))
	}
	var c Config
	if err := yamlStrict(b, &c); err != nil {
		return nil, fmt.Errorf("preset %s: %w", name, err)
	}
	for i := range c.Ignore {
		c.Ignore[i].Name = name + ": " + c.Ignore[i].Name
	}
	return c.Ignore, nil
}

// PresetRules renders a preset for `tgsieve presets`.
func PresetRules(name string) ([]Rule, error) { return LoadPreset(name) }
