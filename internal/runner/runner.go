// Package runner executes terragrunt and collects its structured artifacts.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/textutil"
	"github.com/imcitius/tgsieve/internal/tfplan"
)

const planFileName = "tfplan.json"

type Options struct {
	Dir            string   // working dir
	Binary         string   // terragrunt binary
	All            bool     // pass --all
	TerragruntArgs []string // extra flags for terragrunt itself
	TFArgs         []string // args after "--", e.g. plan -refresh=false
	Command        string   // "plan"
	JSONOutDir     string   // where per-unit tfplan.json land (temp if empty)
	OutDir         string   // where binary plans land (skipped if empty)
	TFPath         string   // --tf-path override
	Progress       *Progress
	Stderr         io.Writer
}

// LogLine is one record of terragrunt's --log-format=json stream.
type LogLine struct {
	Time       string   `json:"time"`
	Level      string   `json:"level"`
	Msg        string   `json:"msg"`
	WorkingDir string   `json:"working-dir"`
	TFPath     string   `json:"tf-path"`
	TFArgs     []string `json:"tf-command-args"`
	Prefix     string   `json:"prefix"`
}

type reportEntry struct {
	Started string `json:"Started"`
	Ended   string `json:"Ended"`
	Name    string `json:"Name"`
	Result  string `json:"Result"`
	Reason  string `json:"Reason"`
	Cmd     string `json:"Cmd"`
}

type Result struct {
	Run      model.Run
	ExitCode int
	Errors   []string
	Duration time.Duration
	PlanDir  string
}

// Run executes terragrunt, streams progress, and parses the produced plans.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Binary == "" {
		opts.Binary = "terragrunt"
	}
	if opts.Command == "" {
		opts.Command = "plan"
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}

	planDir := opts.JSONOutDir
	cleanup := func() {}
	if planDir == "" {
		d, err := os.MkdirTemp("", "tgsieve-plans-")
		if err != nil {
			return nil, err
		}
		planDir = d
		cleanup = func() { os.RemoveAll(d) }
	}
	defer cleanup()

	reportFile := filepath.Join(os.TempDir(), fmt.Sprintf("tgsieve-report-%d.json", time.Now().UnixNano()))
	defer os.Remove(reportFile)

	args := []string{"run"}
	if opts.All {
		args = append(args, "--all")
	}
	args = append(args,
		"--json-out-dir", planDir,
		"--log-format", "json",
		"--report-file", reportFile,
		"--report-format", "json",
		"--summary-disable",
	)
	if opts.OutDir != "" {
		args = append(args, "--out-dir", opts.OutDir)
	}
	if tf := resolveTFPath(opts.TFPath); tf != "" {
		args = append(args, "--tf-path", tf)
	}
	args = append(args, opts.TerragruntArgs...)
	args = append(args, "--", opts.Command)
	args = append(args, opts.TFArgs...)

	cmd := exec.CommandContext(ctx, opts.Binary, args...)
	cmd.Dir = opts.Dir
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", opts.Binary, err)
	}

	res := &Result{PlanDir: planDir}
	var mu sync.Mutex
	var wg sync.WaitGroup
	consume := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ll LogLine
			if err := json.Unmarshal([]byte(line), &ll); err != nil || ll.Level == "" {
				// Not a terragrunt log record (e.g. raw tf output): keep it,
				// but only surface it in verbose mode.
				if opts.Progress != nil {
					opts.Progress.Raw(line)
				}
				continue
			}
			mu.Lock()
			switch ll.Level {
			case "error":
				msg := strings.TrimSpace(ll.Msg)
				if ll.WorkingDir != "" {
					msg = ll.WorkingDir + ": " + msg
				}
				res.Errors = append(res.Errors, msg)
				if opts.Progress != nil {
					// The live feed gets the headline; the full blob is kept
					// for the report at the end.
					head := textutil.Headline(ll.Msg)
					if ll.WorkingDir != "" {
						head = ll.WorkingDir + ": " + head
					}
					opts.Progress.Error(head)
				}
			case "stdout", "stderr":
				// The wall of terraform text we exist to replace.
				if opts.Progress != nil {
					opts.Progress.Unit(ll.WorkingDir)
				}
			default:
				if opts.Progress != nil {
					opts.Progress.Unit(ll.WorkingDir)
					opts.Progress.Note(ll.Level, ll.Msg)
				}
			}
			mu.Unlock()
		}
	}
	wg.Add(2)
	go consume(stdout)
	go consume(stderr)

	var stop chan struct{}
	if opts.Progress != nil {
		stop = opts.Progress.Watch(planDir)
	}
	wg.Wait()
	waitErr := cmd.Wait()
	if stop != nil {
		close(stop)
		opts.Progress.Done()
	}
	res.Duration = time.Since(start)

	var ee *exec.ExitError
	if waitErr != nil {
		if errors.As(waitErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			return res, fmt.Errorf("running %s: %w", opts.Binary, waitErr)
		}
	}

	run, err := Collect(planDir)
	if err != nil {
		return res, err
	}
	run.WorkingDir = opts.Dir
	run.Command = opts.Command
	applyReport(&run, reportFile, res.Errors)
	res.Run = run
	return res, nil
}

// Collect parses every tfplan.json under dir into units.
func Collect(dir string) (model.Run, error) {
	var run model.Run
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != planFileName {
			return nil
		}
		rel, rerr := filepath.Rel(dir, filepath.Dir(path))
		if rerr != nil {
			rel = filepath.Dir(path)
		}
		if rel == "." {
			rel = filepath.Base(dir)
		}
		p, perr := tfplan.ParseFile(path)
		if perr != nil {
			return perr
		}
		run.Units = append(run.Units, tfplan.ToUnit(filepath.ToSlash(rel), p))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return run, err
	}
	return run, nil
}

// applyReport folds terragrunt's run report into the units: durations for the
// ones that ran, plus synthetic units for the ones that failed before writing
// a plan file.
func applyReport(run *model.Run, reportFile string, errs []string) {
	b, err := os.ReadFile(reportFile)
	if err != nil {
		return
	}
	var entries []reportEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return
	}
	byPath := map[string]*model.Unit{}
	for i := range run.Units {
		byPath[run.Units[i].Path] = &run.Units[i]
	}
	for _, e := range entries {
		name := filepath.ToSlash(e.Name)
		u, ok := byPath[name]
		if !ok {
			nu := model.Unit{Path: name}
			switch e.Result {
			case "failed", "early exit":
				nu.Errored = true
				nu.Error = firstErrorFor(name, errs)
				if e.Result == "early exit" {
					nu.Errored = false
					nu.Skipped = true
					nu.Error = "skipped: dependency failed"
				}
			case "excluded":
				nu.Skipped = true
			}
			run.Units = append(run.Units, nu)
			byPath[name] = &run.Units[len(run.Units)-1]
			u = byPath[name]
		}
		if e.Result == "failed" {
			u.Errored = true
			if u.Error == "" {
				u.Error = firstErrorFor(name, errs)
			}
		}
		if s, err1 := time.Parse(time.RFC3339Nano, e.Started); err1 == nil {
			if t, err2 := time.Parse(time.RFC3339Nano, e.Ended); err2 == nil {
				u.Duration = t.Sub(s)
			}
		}
	}
}

func firstErrorFor(unit string, errs []string) string {
	for _, e := range errs {
		if strings.Contains(e, unit) {
			return e
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return "failed"
}

// resolveTFPath keeps terragrunt working on machines that only have terraform:
// terragrunt defaults to tofu, which is often not installed.
func resolveTFPath(override string) string {
	if override != "" {
		return override
	}
	if os.Getenv("TG_TF_PATH") != "" {
		return ""
	}
	if _, err := exec.LookPath("tofu"); err == nil {
		return ""
	}
	if _, err := exec.LookPath("terraform"); err == nil {
		return "terraform"
	}
	return ""
}
