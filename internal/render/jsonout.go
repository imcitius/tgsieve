package render

import (
	"encoding/json"
	"io"

	"github.com/imcitius/tgsieve/internal/sieve"
)

// SchemaVersion is the version of the JSON document below. It is deliberately
// a separate set of types from the internal report: consumers should not break
// because something was renamed inside the sieve.
const SchemaVersion = 1

// Meta is what the command knows about itself.
type Meta struct {
	Version string
	Command string
	Engine  string
}

type jsonDocument struct {
	SchemaVersion int          `json:"schema_version"`
	Tool          jsonTool     `json:"tool"`
	Command       string       `json:"command"`
	Summary       jsonSummary  `json:"summary"`
	Changes       []jsonChange `json:"changes"`
	Failures      []jsonFail   `json:"failures,omitempty"`
	NotRun        []jsonSkip   `json:"not_run,omitempty"`
	Timings       []jsonTiming `json:"timings,omitempty"`
	Applied       *jsonApplied `json:"applied,omitempty"`
}

type jsonTool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Engine  string `json:"engine,omitempty"`
}

type jsonSummary struct {
	Units        jsonUnits    `json:"units"`
	Changes      jsonCounts   `json:"changes"`
	Severity     jsonSeverity `json:"severity"`
	DurationMS   int64        `json:"duration_ms,omitempty"`
	NoRefresh    bool         `json:"no_refresh"`
	Sieved       jsonSieved   `json:"sieved"`
	ExpiredRules []string     `json:"expired_rules,omitempty"`
	TFPath       string       `json:"tf_path,omitempty"`
}

type jsonUnits struct {
	Total     int `json:"total"`
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
	Failed    int `json:"failed"`
	NotRun    int `json:"not_run"`
}

type jsonCounts struct {
	Create            int `json:"create"`
	Update            int `json:"update"`
	Delete            int `json:"delete"`
	Replace           int `json:"replace"`
	Drift             int `json:"drift"`
	DriftNotAddressed int `json:"drift_not_addressed"`
	Total             int `json:"total"`
}

type jsonSeverity struct {
	Level  string         `json:"level,omitempty"`
	Counts map[string]int `json:"counts,omitempty"`
}

type jsonSieved struct {
	Attributes int `json:"attributes"`
	Resources  int `json:"resources"`
	Rules      int `json:"rules"`
	Normalized int `json:"normalized"`
}

type jsonChange struct {
	Action        string     `json:"action"`
	Drift         bool       `json:"drift,omitempty"`
	DriftReverted bool       `json:"drift_reverted,omitempty"`
	Units         []string   `json:"units"`
	Instances     int        `json:"instances"`
	Address       string     `json:"address"`
	Type          string     `json:"type"`
	Name          string     `json:"name"`
	Module        string     `json:"module,omitempty"`
	Attributes    []jsonAttr `json:"attributes,omitempty"`
	HiddenAttrs   int        `json:"hidden_attributes,omitempty"`
	ValuesVary    bool       `json:"values_vary,omitempty"`
}

type jsonAttr struct {
	Path string `json:"path"`
	Kind string `json:"kind,omitempty"`
	// Before and After are omitted entirely when the value is sensitive: a
	// machine-readable report is the easiest place for a secret to end up
	// somewhere it should not be.
	Before        any  `json:"before,omitempty"`
	After         any  `json:"after,omitempty"`
	Sensitive     bool `json:"sensitive,omitempty"`
	AfterUnknown  bool `json:"after_unknown,omitempty"`
	ForcesReplace bool `json:"forces_replace,omitempty"`
	Count         int  `json:"count,omitempty"`
	Varies        bool `json:"varies,omitempty"`
}

type jsonFail struct {
	Headline  string   `json:"headline"`
	Units     []string `json:"units"`
	Count     int      `json:"count"`
	Locations []string `json:"locations,omitempty"`
	Detail    []string `json:"detail,omitempty"`
}

type jsonSkip struct {
	Unit   string `json:"unit"`
	Reason string `json:"reason"`
}

type jsonTiming struct {
	Unit    string `json:"unit"`
	MS      int64  `json:"ms"`
	Changes int    `json:"changes"`
	Reused  bool   `json:"reused,omitempty"`
}

// Applied describes what an apply did.
type Applied = jsonApplied

type jsonApplied struct {
	OK         bool     `json:"ok"`
	DurationMS int64    `json:"duration_ms"`
	Units      []string `json:"units"`
	Errors     []string `json:"errors,omitempty"`
}

// JSON writes the machine-readable report.
func JSON(w io.Writer, rep *sieve.Report, meta Meta, applied *jsonApplied) error {
	doc := jsonDocument{
		SchemaVersion: SchemaVersion,
		Tool:          jsonTool{Name: "tgsieve", Version: meta.Version, Engine: meta.Engine},
		Command:       meta.Command,
		Summary: jsonSummary{
			Units: jsonUnits{
				Total:     rep.UnitsTotal,
				Changed:   rep.UnitsChanged,
				Unchanged: len(rep.UnchangedUnits),
				Failed:    len(rep.ErroredUnits),
				NotRun:    len(rep.SkippedUnits),
			},
			Changes: jsonCounts{
				Create:            rep.Kept.Create,
				Update:            rep.Kept.Update,
				Delete:            rep.Kept.Delete,
				Replace:           rep.Kept.Replace,
				Drift:             rep.Kept.Drift,
				DriftNotAddressed: rep.Kept.DriftLeft,
				Total:             rep.Kept.Total(),
			},
			Severity:     jsonSeverity{Level: rep.Severity, Counts: rep.SeverityCounts},
			NoRefresh:    rep.NoRefresh,
			ExpiredRules: rep.ExpiredRules,
			TFPath:       rep.TFPath,
			Sieved: jsonSieved{
				Attributes: rep.HiddenAttrs,
				Resources:  rep.HiddenResources,
				Rules:      len(rep.RuleStats),
				Normalized: rep.Normalized,
			},
		},
		Changes: make([]jsonChange, 0, len(rep.Groups)),
		Applied: applied,
	}
	if rep.Wall > 0 {
		doc.Summary.DurationMS = rep.Wall.Milliseconds()
	}

	for _, g := range rep.Groups {
		c := jsonChange{
			Action:        string(g.Action),
			Drift:         g.Drift,
			DriftReverted: g.Sample.DriftReverted,
			Units:         g.Units,
			Instances:     g.Instances(),
			Address:       g.Sample.BaseAddress,
			Type:          g.Sample.Type,
			Name:          g.Sample.Name,
			Module:        g.Sample.Module,
			HiddenAttrs:   len(g.Sample.Hidden),
			ValuesVary:    g.ValueVary,
		}
		for i, a := range g.Sample.Attrs {
			attr := jsonAttr{
				Path:          a.Path,
				Kind:          a.Kind,
				Sensitive:     a.Sensitive,
				AfterUnknown:  a.AfterUnknown,
				ForcesReplace: a.ForcesReplace,
				Count:         a.Count,
			}
			if i < len(g.Varies) && g.Varies[i] {
				attr.Varies = true
			}
			if !a.Sensitive {
				attr.Before, attr.After = a.Before, a.After
			}
			c.Attributes = append(c.Attributes, attr)
		}
		doc.Changes = append(doc.Changes, c)
	}

	for _, f := range rep.Failures {
		doc.Failures = append(doc.Failures, jsonFail{
			Headline: f.Headline, Units: f.Units, Count: f.Count,
			Locations: f.Locations, Detail: f.Detail,
		})
	}
	for _, s := range rep.SkippedUnits {
		doc.NotRun = append(doc.NotRun, jsonSkip{Unit: s.Path, Reason: s.Reason})
	}
	for _, t := range rep.Timings {
		doc.Timings = append(doc.Timings, jsonTiming{
			Unit: t.Path, MS: t.Duration.Milliseconds(), Changes: t.Changes, Reused: t.Reused,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// AppliedResult describes what an apply did, for the JSON document.
func AppliedResult(ok bool, ms int64, units []string, errs []string) *jsonApplied {
	return &jsonApplied{OK: ok, DurationMS: ms, Units: units, Errors: errs}
}
