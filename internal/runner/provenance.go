package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/imcitius/tgsieve/internal/model"
)

// ProvenanceFile sits beside the saved plans and records what they were made
// from. --resume reuses plans it did not produce, and a plan is only valid for
// the configuration that produced it.
const ProvenanceFile = ".tgsieve-run.json"

type Provenance struct {
	Created time.Time `json:"created"`
	Command string    `json:"command"`
	// Source names what the fingerprint was taken from: "git" or "files".
	Source string `json:"source,omitempty"`
	// Tool is the tgsieve version that wrote the plans.
	Tool string `json:"tool,omitempty"`
	// Commit is the git HEAD the plans were produced at, empty outside a repo.
	Commit string `json:"commit,omitempty"`
	// Tree fingerprints uncommitted changes, so an unchanged dirty tree still
	// counts as the same generation while an edited one does not.
	Tree  string `json:"tree,omitempty"`
	Dirty bool   `json:"dirty,omitempty"`
	// Sources maps each unit to its resolved module source, so a remote module
	// that moved is not mistaken for an unchanged stack.
	Sources map[string]string `json:"sources,omitempty"`
}

// SourceChanges names the units whose module source differs between two
// recordings. Units missing from either side are skipped: a source that could
// not be resolved is unknown, not unchanged.
func (p Provenance) SourceChanges(q Provenance) []string {
	var moved []string
	for unit, was := range p.Sources {
		if now, ok := q.Sources[unit]; ok && now != was {
			moved = append(moved, unit)
		}
	}
	sort.Strings(moved)
	return moved
}

// SameGeneration reports whether plans recorded as p may be mixed with a run
// happening in state q.
func (p Provenance) SameGeneration(q Provenance) bool {
	return p.Commit == q.Commit && p.Tree == q.Tree && len(p.SourceChanges(q)) == 0
}

// Describe renders the difference for an error message.
func (p Provenance) Describe() string {
	switch {
	case p.Source == "files":
		return "config fingerprint " + p.Tree
	case p.Commit == "":
		return "an unidentifiable state"
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
// Outside a git repository it falls back to hashing the configuration itself,
// so a directory pulled from a --source URL or unpacked from an archive still
// has an identity that changes when the code does.
func CurrentProvenance(ctx context.Context, dir, command, tool string) Provenance {
	p := Provenance{Created: time.Now(), Command: command, Tool: tool}
	commit, err := gitOutput(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		p.Source = "files"
		p.Tree = fingerprintConfigs(dir)
		return p
	}
	p.Source = "git"
	p.Commit = strings.TrimSpace(commit)
	status, err := gitOutput(ctx, dir, "status", "--porcelain")
	if err != nil {
		return p
	}
	tracked := meaningfulChanges(status)
	p.Dirty = tracked != ""
	// Units written by `terragrunt stack generate` live in .terragrunt-stack,
	// which is normally gitignored — so git alone would report an unchanged
	// tree after the generated units were replaced.
	sum := sha256.Sum256([]byte(tracked + "\x00" + fingerprintGenerated(dir)))
	p.Tree = hex.EncodeToString(sum[:])[:16]
	return p
}

// GeneratedUnitsDir holds units materialized from a terragrunt.stack.hcl.
const GeneratedUnitsDir = ".terragrunt-stack"

// fingerprintGenerated hashes the units a stack file generated, which git does
// not see.
func fingerprintGenerated(dir string) string {
	h := sha256.New()
	var roots []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		switch d.Name() {
		case ".git", ".terragrunt-cache", ".terraform":
			return filepath.SkipDir
		case GeneratedUnitsDir:
			roots = append(roots, p)
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(roots)
	for _, root := range roots {
		rel, err := filepath.Rel(dir, root)
		if err != nil {
			rel = root
		}
		fmt.Fprintf(h, "%s\x00%s\x00", filepath.ToSlash(rel), fingerprintConfigs(root))
	}
	if len(roots) == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
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

// configExts are the files that decide what a plan will contain.
var configExts = []string{".hcl", ".tf", ".tfvars", ".tftpl", ".json", ".yaml", ".yml"}

// fingerprintConfigs hashes every configuration file under dir. It is the
// no-git fallback, so it errs towards including too much rather than missing a
// change: an empty fingerprint would silently make every generation "equal".
func fingerprintConfigs(dir string) string {
	h := sha256.New()
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || generated(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if generated(name) {
			// Plans are .json and would otherwise move the fingerprint every
			// run — the same trap the git path had with untracked artifacts.
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		for _, want := range configExts {
			if ext == want {
				files = append(files, p)
				break
			}
		}
		return nil
	})
	sort.Strings(files)
	for _, f := range files {
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			rel = f
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s\x00%x\x00", filepath.ToSlash(rel), sha256.Sum256(b))
	}
	if len(files) == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// TimingsFile remembers how long each unit took, across invocations. After a
// --resume the current run only knows about the units it re-ran, and a
// "slowest unit" drawn from that subset would be a lie about the stack.
const TimingsFile = ".tgsieve-timings.json"

type savedTiming struct {
	Millis   int64     `json:"ms"`
	Recorded time.Time `json:"at"`
}

// SaveTimings merges this run's durations into the record beside the plans.
func SaveTimings(dir string, run model.Run) error {
	saved := loadTimings(dir)
	for _, u := range run.Units {
		if u.Duration <= 0 {
			continue
		}
		saved[u.Path] = savedTiming{Millis: u.Duration.Milliseconds(), Recorded: time.Now()}
	}
	b, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, TimingsFile), append(b, '\n'), 0o644)
}

// timingTTL is how long a recorded duration is worth believing. A unit that
// has since been split, moved or sped up should stop being reported as the
// slowest in the stack on the strength of a stale measurement.
const timingTTL = 14 * 24 * time.Hour

// ApplyTimings fills in durations for units this invocation did not run, and
// marks them as reused so the report can say so.
func ApplyTimings(dir string, run *model.Run) {
	saved := loadTimings(dir)
	if len(saved) == 0 {
		return
	}
	for i := range run.Units {
		u := &run.Units[i]
		if u.Duration > 0 {
			continue
		}
		t, ok := saved[u.Path]
		if !ok || expired(t) {
			continue
		}
		u.Duration = time.Duration(t.Millis) * time.Millisecond
		u.Reused = true
		u.TimedAt = t.Recorded
	}
}

func expired(t savedTiming) bool {
	return !t.Recorded.IsZero() && time.Since(t.Recorded) > timingTTL
}

func loadTimings(dir string) map[string]savedTiming {
	out := map[string]savedTiming{}
	b, err := os.ReadFile(filepath.Join(dir, TimingsFile))
	if err != nil {
		return out
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]savedTiming{}
	}
	// Drop expired entries on read, so the file also stops growing.
	for k, v := range out {
		if expired(v) {
			delete(out, k)
		}
	}
	return out
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
