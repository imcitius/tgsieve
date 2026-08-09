package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProvenanceFile sits beside the saved plans and records what they were made
// from. --resume reuses plans it did not produce, and a plan is only valid for
// the configuration that produced it.
const ProvenanceFile = ".tgsieve-run.json"

type Provenance struct {
	Created time.Time `json:"created"`
	Command string    `json:"command"`
	// Commit is the git HEAD the plans were produced at, empty outside a repo.
	Commit string `json:"commit,omitempty"`
	// Tree fingerprints uncommitted changes, so an unchanged dirty tree still
	// counts as the same generation while an edited one does not.
	Tree  string `json:"tree,omitempty"`
	Dirty bool   `json:"dirty,omitempty"`
}

// SameGeneration reports whether plans recorded as p may be mixed with a run
// happening in state q.
func (p Provenance) SameGeneration(q Provenance) bool {
	return p.Commit == q.Commit && p.Tree == q.Tree
}

// Describe renders the difference for an error message.
func (p Provenance) Describe() string {
	switch {
	case p.Commit == "":
		return "no git repository"
	case p.Dirty:
		return shortSHA(p.Commit) + " with uncommitted changes"
	default:
		return shortSHA(p.Commit)
	}
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// CurrentProvenance inspects the working tree the run is about to plan from.
func CurrentProvenance(ctx context.Context, dir, command string) Provenance {
	p := Provenance{Created: time.Now(), Command: command}
	commit, err := gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return p
	}
	p.Commit = strings.TrimSpace(commit)
	status, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err != nil {
		return p
	}
	tracked := meaningfulChanges(status)
	p.Dirty = tracked != ""
	sum := sha256.Sum256([]byte(tracked))
	p.Tree = hex.EncodeToString(sum[:])[:16]
	return p
}

// meaningfulChanges drops the working-tree entries a run creates for itself.
// Planning writes caches, lock files and state; if those counted, no two runs
// would ever agree on the generation and --resume could never fire.
func meaningfulChanges(status string) string {
	var kept []string
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		path := line
		if len(line) > 3 {
			path = line[3:]
		}
		if generated(strings.Trim(path, `"`)) {
			continue
		}
		kept = append(kept, line)
	}
	sort.Strings(kept)
	return strings.Join(kept, "\n")
}

var generatedDirs = []string{".terragrunt-cache", ".terraform"}

var generatedSuffixes = []string{
	".tfplan", ".tfplan.json", ".tfstate", ".tfstate.backup", "tfplan.json",
}

func generated(path string) bool {
	// A rename entry is "old -> new"; either side being generated is enough.
	if i := strings.Index(path, " -> "); i >= 0 {
		return generated(path[:i]) || generated(path[i+4:])
	}
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		for _, d := range generatedDirs {
			if seg == d {
				return true
			}
		}
	}
	for _, suf := range generatedSuffixes {
		if strings.HasSuffix(path, suf) {
			return true
		}
	}
	return false
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// WriteProvenance records the current state next to the plans.
func WriteProvenance(dir string, p Provenance) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ProvenanceFile), append(b, '\n'), 0o644)
}

// ReadProvenance loads what a previous run recorded. A missing file is
// reported as such, because plans of unknown origin are exactly what --resume
// must not silently trust.
func ReadProvenance(dir string) (Provenance, error) {
	var p Provenance
	b, err := os.ReadFile(filepath.Join(dir, ProvenanceFile))
	if err != nil {
		if os.IsNotExist(err) {
			return p, fmt.Errorf("%s has no %s: these plans were not written by this version of tgsieve", dir, ProvenanceFile)
		}
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%s: %w", filepath.Join(dir, ProvenanceFile), err)
	}
	return p, nil
}
