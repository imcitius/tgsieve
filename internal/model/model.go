// Package model holds the normalized, tool-agnostic representation of a
// terragrunt run: units, resource changes and attribute-level diffs.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

type Action string

const (
	ActionNoOp    Action = "no-op"
	ActionCreate  Action = "create"
	ActionRead    Action = "read"
	ActionUpdate  Action = "update"
	ActionDelete  Action = "delete"
	ActionReplace Action = "replace"
	ActionForget  Action = "forget"
)

// Severity is the default ranking used for ordering and for deciding what
// never gets hidden.
func (a Action) Severity() int {
	switch a {
	case ActionDelete, ActionReplace:
		return 3
	case ActionUpdate:
		return 2
	case ActionCreate:
		return 1
	default:
		return 0
	}
}

func (a Action) Symbol() string {
	switch a {
	case ActionCreate:
		return "+"
	case ActionUpdate:
		return "~"
	case ActionDelete:
		return "-"
	case ActionReplace:
		return "±"
	case ActionRead:
		return "<"
	case ActionForget:
		return "."
	default:
		return " "
	}
}

// Unknown is the placeholder for values only known after apply.
type Unknown struct{}

func (Unknown) String() string { return "(known after apply)" }

// Sensitive is the placeholder for values terraform marked sensitive.
type Sensitive struct{}

func (Sensitive) String() string { return "(sensitive)" }

// Kinds of attribute change beyond a plain before/after.
const (
	KindChanged   = ""          // a value became another value
	KindReordered = "reordered" // the same elements in a different order
	KindAdded     = "added"     // an element appeared in a collection
	KindRemoved   = "removed"   // an element left a collection
)

// AttrChange is one changed attribute inside a resource.
type AttrChange struct {
	Path          string `json:"path"`
	Kind          string `json:"kind,omitempty"`
	Count         int    `json:"count,omitempty"` // elements involved, for reordering
	Before        any    `json:"before,omitempty"`
	After         any    `json:"after,omitempty"`
	AfterUnknown  bool   `json:"after_unknown,omitempty"`
	Sensitive     bool   `json:"sensitive,omitempty"`
	ForcesReplace bool   `json:"forces_replace,omitempty"`
}

// HiddenAttr records an attribute that a sieve rule removed, so --explain can
// show the user exactly what they are not being shown and why.
type HiddenAttr struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// Resource is one resource change in one unit.
type Resource struct {
	Unit        string `json:"unit"`
	Address     string `json:"address"`
	BaseAddress string `json:"base_address"` // address with the [index] stripped
	Module      string `json:"module,omitempty"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Index       any    `json:"index,omitempty"`
	Provider    string `json:"provider,omitempty"`
	// Mode is "managed" for resources and "data" for data sources. A data
	// source is read, never created, and saying otherwise misdescribes what
	// an apply will do.
	Mode   string       `json:"mode,omitempty"`
	Action Action       `json:"action"`
	Attrs  []AttrChange `json:"attrs,omitempty"`
	Drift  bool         `json:"drift,omitempty"`
	// DriftReverted marks drift the plan will undo. Drift the plan does not
	// address is the kind that survives an apply, and is worth separating.
	DriftReverted bool `json:"drift_reverted,omitempty"`
	Imported      bool `json:"imported,omitempty"`

	Hidden []HiddenAttr `json:"hidden,omitempty"`
}

// Shape identifies "the same kind of change" regardless of the concrete values
// or which unit/instance it happened in. Used for cross-unit collapsing.
func (r Resource) Shape() string {
	h := sha256.New()
	h.Write([]byte(string(r.Action) + "\x00" + r.Type + "\x00" + r.Name + "\x00" + r.Module + "\x00"))
	paths := make([]string, 0, len(r.Attrs))
	for _, a := range r.Attrs {
		paths = append(paths, a.Path)
	}
	sort.Strings(paths)
	h.Write([]byte(strings.Join(paths, "\x00")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ValueShape additionally pins the before/after values, so two resources with
// the same ValueShape are literally the same diff.
func (r Resource) ValueShape() string {
	type kv struct {
		P string `json:"p"`
		B any    `json:"b"`
		A any    `json:"a"`
		U bool   `json:"u"`
	}
	attrs := make([]kv, 0, len(r.Attrs))
	for _, a := range r.Attrs {
		attrs = append(attrs, kv{a.Path, a.Before, a.After, a.AfterUnknown})
	}
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].P < attrs[j].P })
	b, _ := json.Marshal(struct {
		Act   Action `json:"act"`
		Type  string `json:"type"`
		Name  string `json:"name"`
		Mod   string `json:"mod"`
		Attrs []kv   `json:"attrs"`
	}{r.Action, r.Type, r.Name, r.Module, attrs})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// Unit is one terragrunt unit (a directory with a terragrunt.hcl).
type Unit struct {
	Path      string       `json:"path"`
	Resources []Resource   `json:"resources,omitempty"`
	Outputs   []AttrChange `json:"outputs,omitempty"`
	Errored   bool         `json:"errored,omitempty"`
	Error     string       `json:"error,omitempty"`
	// Errors holds every diagnostic for this unit. One failure can produce
	// hundreds — one per orphaned resource, say — and reporting only the first
	// hides how big the problem is.
	Errors   []string      `json:"errors,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`
	Skipped  bool          `json:"skipped,omitempty"`
	// Reused marks a unit whose plan came from an earlier invocation.
	Reused bool `json:"reused,omitempty"`
	// TimedAt is when a reused duration was measured.
	TimedAt time.Time `json:"timed_at,omitempty"`
}

type Counts struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Delete  int `json:"delete"`
	Replace int `json:"replace"`
	Drift   int `json:"drift"`
	// DriftLeft counts drift this plan will not put back.
	DriftLeft int `json:"drift_left"`
	NoOp      int `json:"no_op"`
}

func (c Counts) Total() int { return c.Create + c.Update + c.Delete + c.Replace }

func (c *Counts) Add(a Action, drift bool) {
	if drift {
		c.Drift++
		return
	}
	switch a {
	case ActionCreate:
		c.Create++
	case ActionUpdate:
		c.Update++
	case ActionDelete:
		c.Delete++
	case ActionReplace:
		c.Replace++
	case ActionNoOp:
		c.NoOp++
	}
}

// Run is everything one `tgsieve plan` produced, before sieving.
type Run struct {
	Units      []Unit `json:"units"`
	WorkingDir string `json:"working_dir"`
	Command    string `json:"command"`
}

func (r Run) Counts() Counts {
	var c Counts
	for _, u := range r.Units {
		for _, res := range u.Resources {
			c.Add(res.Action, res.Drift)
		}
	}
	return c
}
