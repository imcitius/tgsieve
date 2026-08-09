// Package tfplan parses `terraform show -json` plan files (the format
// terragrunt writes into --json-out-dir) into the normalized model.
package tfplan

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/imcitius/tgsieve/internal/model"
)

type Plan struct {
	FormatVersion    string            `json:"format_version"`
	TerraformVersion string            `json:"terraform_version"`
	ResourceChanges  []ResourceChange  `json:"resource_changes"`
	ResourceDrift    []ResourceChange  `json:"resource_drift"`
	OutputChanges    map[string]Change `json:"output_changes"`
	Errored          bool              `json:"errored"`
	Applyable        *bool             `json:"applyable"`
	Complete         *bool             `json:"complete"`
}

type ResourceChange struct {
	Address       string `json:"address"`
	ModuleAddress string `json:"module_address"`
	Mode          string `json:"mode"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	Index         any    `json:"index"`
	ProviderName  string `json:"provider_name"`
	Deposed       string `json:"deposed"`
	Change        Change `json:"change"`
}

type Importing struct {
	ID string `json:"id"`
}

type Change struct {
	Actions         []string   `json:"actions"`
	Before          any        `json:"before"`
	After           any        `json:"after"`
	AfterUnknown    any        `json:"after_unknown"`
	BeforeSensitive any        `json:"before_sensitive"`
	AfterSensitive  any        `json:"after_sensitive"`
	ReplacePaths    [][]any    `json:"replace_paths"`
	Importing       *Importing `json:"importing"`
}

// ParseFile reads one tfplan.json.
func ParseFile(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

var indexRe = regexp.MustCompile(`\[[^\]]*\]$`)

// BaseAddress strips a trailing [0] / ["key"] from a resource address.
func BaseAddress(addr string) string { return indexRe.ReplaceAllString(addr, "") }

// ToUnit converts a parsed plan into a model.Unit for the given unit path.
func ToUnit(unitPath string, p *Plan) model.Unit {
	u := model.Unit{Path: unitPath, Errored: p.Errored}
	for _, rc := range p.ResourceChanges {
		if r, ok := toResource(unitPath, rc, false); ok {
			u.Resources = append(u.Resources, r)
		}
	}
	// Drift the plan will put back is a different problem from drift it leaves
	// alone: the second survives the apply.
	planned := make(map[string]bool, len(p.ResourceChanges))
	for _, rc := range p.ResourceChanges {
		if actionOf(rc.Change.Actions) != model.ActionNoOp {
			planned[rc.Address] = true
		}
	}
	for _, rc := range p.ResourceDrift {
		if r, ok := toResource(unitPath, rc, true); ok {
			r.DriftReverted = planned[rc.Address]
			u.Resources = append(u.Resources, r)
		}
	}
	for name, ch := range p.OutputChanges {
		if actionOf(ch.Actions) == model.ActionNoOp {
			continue
		}
		out := model.AttrChange{Path: name}
		if attrs := Diff(ch); len(attrs) > 0 {
			out.Before, out.After = attrs[0].Before, attrs[0].After
			out.AfterUnknown, out.Sensitive = attrs[0].AfterUnknown, attrs[0].Sensitive
		}
		u.Outputs = append(u.Outputs, out)
	}
	sort.Slice(u.Outputs, func(i, j int) bool { return u.Outputs[i].Path < u.Outputs[j].Path })
	sort.SliceStable(u.Resources, func(i, j int) bool { return u.Resources[i].Address < u.Resources[j].Address })
	return u
}

func toResource(unitPath string, rc ResourceChange, drift bool) (model.Resource, bool) {
	act := actionOf(rc.Change.Actions)
	if act == model.ActionNoOp && !drift {
		// Drop no-ops entirely: no signal, and in a heavy stack they are the
		// bulk of the payload.
		return model.Resource{}, false
	}
	r := model.Resource{
		Unit:        unitPath,
		Address:     rc.Address,
		BaseAddress: BaseAddress(rc.Address),
		Module:      rc.ModuleAddress,
		Type:        rc.Type,
		Name:        rc.Name,
		Index:       rc.Index,
		Provider:    shortProvider(rc.ProviderName),
		Action:      act,
		Drift:       drift,
		Imported:    rc.Change.Importing != nil,
	}
	r.Attrs = Diff(rc.Change)
	markReplacePaths(&r, rc.Change.ReplacePaths)
	return r, true
}

func shortProvider(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func actionOf(actions []string) model.Action {
	switch len(actions) {
	case 0:
		return model.ActionNoOp
	case 1:
		switch actions[0] {
		case "create":
			return model.ActionCreate
		case "update":
			return model.ActionUpdate
		case "delete":
			return model.ActionDelete
		case "read":
			return model.ActionRead
		case "forget":
			return model.ActionForget
		default:
			return model.ActionNoOp
		}
	default:
		// ["delete","create"] or ["create","delete"] (create_before_destroy).
		has := map[string]bool{}
		for _, a := range actions {
			has[a] = true
		}
		switch {
		case has["delete"] && has["create"]:
			return model.ActionReplace
		case has["delete"]:
			return model.ActionDelete
		default:
			return model.ActionUpdate
		}
	}
}

func markReplacePaths(r *model.Resource, paths [][]any) {
	if len(paths) == 0 {
		return
	}
	prefixes := make([]string, 0, len(paths))
	for _, p := range paths {
		prefixes = append(prefixes, joinPath(p))
	}
	for i := range r.Attrs {
		for _, pre := range prefixes {
			if r.Attrs[i].Path == pre || strings.HasPrefix(r.Attrs[i].Path, pre+".") {
				r.Attrs[i].ForcesReplace = true
				break
			}
		}
	}
}

func joinPath(p []any) string {
	parts := make([]string, 0, len(p))
	for _, seg := range p {
		switch v := seg.(type) {
		case string:
			parts = append(parts, v)
		case float64:
			parts = append(parts, fmt.Sprintf("%d", int(v)))
		default:
			parts = append(parts, fmt.Sprint(v))
		}
	}
	return strings.Join(parts, ".")
}
