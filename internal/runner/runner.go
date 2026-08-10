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
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/imcitius/tgsieve/internal/config"
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
	Filters        []string // --filter queries (--all only)
	FilterAffected bool     // --filter-affected (--all only)
	Parallelism    int      // --parallelism (--all only)
	KnownUnits     []string // queue already discovered by the caller
	NoResolveRefs  bool     // skip resolving git refs to commits (offline)
	Engine         string   // "" for terragrunt, "terraform" to drive it directly
	Init           bool     // run init first (direct engine only)
	Progress       *Progress
	Stderr         io.Writer
}

// filterArgs renders the queue filters shared by `find` and `run --all`.
func (o Options) filterArgs() []string {
	var args []string
	for _, f := range o.Filters {
		args = append(args, "--filter", f)
	}
	if o.FilterAffected {
		args = append(args, "--filter-affected")
	}
	return args
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

// TFEvent is one record of terraform's own `-json` stream. A single-unit run
// asks for it so progress can be reported per resource; a stack run cannot,
// because terragrunt forwards these lines unlabelled and units interleave.
type TFEvent struct {
	Level   string `json:"@level"`
	Message string `json:"@message"`
	Type    string `json:"type"`
	Change  struct {
		Resource struct {
			Addr string `json:"addr"`
		} `json:"resource"`
		Action string `json:"action"`
	} `json:"change"`
	Hook struct {
		Resource struct {
			Addr string `json:"addr"`
		} `json:"resource"`
		Action string `json:"action"`
	} `json:"hook"`
	Diagnostic struct {
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		Detail   string `json:"detail"`
		// Range and Snippet carry where the problem is. Five diagnostics that
		// share a summary and differ only by line are five different things to
		// fix, and the line is the part a reader acts on.
		Range struct {
			Filename string `json:"filename"`
			Start    struct {
				Line int `json:"line"`
			} `json:"start"`
		} `json:"range"`
		Snippet struct {
			Context   string `json:"context"`
			Code      string `json:"code"`
			StartLine int    `json:"start_line"`
			Values    []struct {
				Traversal string `json:"traversal"`
				Statement string `json:"statement"`
			} `json:"values"`
		} `json:"snippet"`
	} `json:"diagnostic"`
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
	// durations records how long each unit took when no run report exists,
	// which is the case when terragrunt is not involved.
	durations map[string]time.Duration

	// TFPath is the binary terragrunt actually used, as terragrunt reported
	// it. Which binary ran is the first thing worth knowing when a whole stack
	// fails to initialize, and nothing else in the output says it.
	TFPath      string
	Run         model.Run
	ExitCode    int
	Errors      []string
	Duration    time.Duration
	PlanDir     string
	Interrupted bool
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

	res := &Result{PlanDir: planDir}
	start := time.Now()

	if opts.Progress != nil {
		// Knowing the size of the queue up front turns "7 units seen" into
		// "7/28", which is the difference between a spinner and progress.
		if opts.All {
			units := opts.KnownUnits
			if len(units) == 0 {
				units, _ = Discover(ctx, opts)
			}
			if len(units) > 0 {
				opts.Progress.SetTotal(len(units))
			}
		} else {
			opts.Progress.SetTotal(1)
		}
	}

	var stop chan struct{}
	if opts.Progress != nil {
		stop = opts.Progress.Watch(planDir)
	}
	var runErr error
	switch {
	case opts.Direct():
		runErr = runDirect(ctx, opts, planDir, res)
	case opts.All:
		runErr = runStack(ctx, opts, planDir, reportFile, res)
	default:
		runErr = runUnit(ctx, opts, planDir, reportFile, res)
	}
	if stop != nil {
		close(stop)
		opts.Progress.Done()
	}
	res.Duration = time.Since(start)
	res.Interrupted = ctx.Err() != nil
	if runErr != nil && !res.Interrupted {
		return res, runErr
	}

	if opts.Direct() && opts.Command == "apply" {
		// A direct apply has no plan document to collect; the run itself is
		// the whole story.
		res.Run.WorkingDir = opts.Dir
		res.Run.Command = opts.Command
		return res, nil
	}

	run, err := Collect(planDir)
	if err != nil {
		return res, err
	}
	run.WorkingDir = opts.Dir
	run.Command = opts.Command
	if opts.Direct() {
		for i := range run.Units {
			if d, ok := res.durations[run.Units[i].Path]; ok {
				run.Units[i].Duration = d
			}
		}
		if len(run.Units) == 0 && (res.ExitCode != 0 || len(res.Errors) > 0) {
			run.Units = []model.Unit{{
				Path: unitName(opts.Dir), Errored: true,
				Error:  firstErrorFor(unitName(opts.Dir), res.Errors),
				Errors: res.Errors,
			}}
		}
	} else {
		applyReport(&run, reportFile, res.Errors, opts.All)
	}
	if !opts.All {
		ensureUnit(&run, opts, res)
	}
	if res.Interrupted {
		markInterrupted(&run)
	}
	res.Run = run
	return res, nil
}

// runStack is the --all path: terragrunt writes one tfplan.json per unit into
// --json-out-dir for us.
func runStack(ctx context.Context, opts Options, planDir, reportFile string, res *Result) error {
	args := []string{"run", "--all",
		// tgsieve does the asking — with its own destroy confirmation — and the
		// child has no terminal to ask through, so terragrunt's own prompt
		// would only ever read EOF and abort.
		"--non-interactive",
		"--report-file", reportFile,
		"--report-format", "json",
		"--summary-disable",
	}
	if opts.Command == "plan" {
		// --json-out-dir is a plan-time artifact; apply reads the binary plans
		// from --out-dir instead.
		args = append(args, "--json-out-dir", planDir)
	}
	if opts.OutDir != "" {
		args = append(args, "--out-dir", opts.OutDir)
	}
	if opts.Parallelism > 0 {
		args = append(args, "--parallelism", strconv.Itoa(opts.Parallelism))
	}
	args = append(args, opts.filterArgs()...)
	args = append(args, opts.TerragruntArgs...)
	args = append(args, "--", opts.Command)
	args = append(args, opts.TFArgs...)
	return stream(ctx, opts, res, args)
}

// runUnit is the single-unit path. --json-out-dir and --out-dir only apply to
// --all runs, so here we drive it ourselves: plan to a binary file, then ask
// terragrunt to render that file as JSON.
func runUnit(ctx context.Context, opts Options, planDir, reportFile string, res *Result) error {
	if opts.Command == "apply" {
		return applyUnit(ctx, opts, reportFile, res)
	}
	planFile, err := binaryPlanPath(opts)
	if err != nil {
		return err
	}
	tfArgs, userOut := splitOutArg(opts.TFArgs)
	if userOut != "" {
		planFile = userOut
	} else if opts.OutDir == "" {
		defer os.Remove(planFile)
	}

	args := []string{"run",
		"--non-interactive",
		"--report-file", reportFile,
		"--report-format", "json",
		"--summary-disable",
	}
	args = append(args, opts.TerragruntArgs...)
	args = append(args, "--", opts.Command, "-out="+planFile)
	if !hasJSONFlag(tfArgs) {
		// Drives the per-resource progress counter. Safe here because only one
		// unit is running, so nothing interleaves with it.
		args = append(args, "-json")
	}
	args = append(args, tfArgs...)
	if err := stream(ctx, opts, res, args); err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return nil // the unit failed; the error lines already tell the story
	}

	show := []string{"run", "--log-disable", "--tf-forward-stdout", "--non-interactive"}
	show = append(show, opts.TerragruntArgs...)
	show = append(show, "--", "show", "-json", planFile)
	out, err := output(ctx, opts, show)
	if err != nil {
		return fmt.Errorf("rendering the plan as JSON: %w", err)
	}
	unitDir := filepath.Join(planDir, unitName(opts.Dir))
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(unitDir, planFileName), out, 0o644)
}

func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "-json" || a == "--json" {
			return true
		}
	}
	return false
}

// applyUnit applies the plan file saved for this unit, so a single-unit apply
// carries the same guarantee as a stack one: what runs is what was reviewed.
func applyUnit(ctx context.Context, opts Options, reportFile string, res *Result) error {
	planFile, err := savedPlanPath(opts)
	if err != nil {
		return err
	}
	if _, err := os.Stat(planFile); err != nil {
		return fmt.Errorf("no saved plan for this unit at %s", planFile)
	}
	args := []string{"run",
		"--non-interactive",
		"--report-file", reportFile,
		"--report-format", "json",
		"--summary-disable",
	}
	args = append(args, opts.TerragruntArgs...)
	args = append(args, "--", "apply", planFile)
	return stream(ctx, opts, res, args)
}

// savedPlanPath is where a unit's binary plan lives, laid out the same way
// terragrunt lays them out for a stack.
func savedPlanPath(opts Options) (string, error) {
	abs, err := filepath.Abs(opts.OutDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, unitName(opts.Dir), "tfplan.tfplan"), nil
}

func binaryPlanPath(opts Options) (string, error) {
	if opts.OutDir != "" {
		abs, err := filepath.Abs(filepath.Join(opts.OutDir, unitName(opts.Dir)))
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
		return filepath.Join(abs, "tfplan.tfplan"), nil
	}
	f, err := os.CreateTemp("", "tgsieve-*.tfplan")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	return name, nil
}

// splitOutArg honours a -out the caller passed through themselves.
func splitOutArg(tfArgs []string) (rest []string, out string) {
	for i := 0; i < len(tfArgs); i++ {
		a := tfArgs[i]
		switch {
		case strings.HasPrefix(a, "-out="), strings.HasPrefix(a, "--out="):
			out = a[strings.Index(a, "=")+1:]
		case a == "-out" || a == "--out":
			if i+1 < len(tfArgs) {
				out = tfArgs[i+1]
				i++
			}
		default:
			rest = append(rest, a)
		}
	}
	if out != "" {
		if abs, err := filepath.Abs(out); err == nil {
			out = abs
		}
	}
	return rest, out
}

// unitName labels a single-unit run the same way an --all run would: relative
// to the project root, so "envs/prod/a" rather than "a".
func unitName(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil || abs == string(filepath.Separator) {
		return "unit"
	}
	if root, err := config.ProjectRoot(abs); err == nil {
		if rel, err := filepath.Rel(root, abs); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(abs)
}

// ensureUnit makes sure a single-unit run that failed before producing a plan
// still shows up as a failed unit rather than vanishing.
func ensureUnit(run *model.Run, opts Options, res *Result) {
	if len(run.Units) > 0 {
		return
	}
	u := model.Unit{Path: unitName(opts.Dir)}
	if res.ExitCode != 0 || len(res.Errors) > 0 {
		u.Errored = true
		u.Error = firstErrorFor(u.Path, res.Errors)
		u.Errors = res.Errors
	}
	run.Units = append(run.Units, u)
}

func newCmd(ctx context.Context, opts Options, args []string) *exec.Cmd {
	full := args
	// Only `run` takes --tf-path; `find` rejects it.
	if args[0] == "run" {
		if tf := resolveTFPath(opts.TFPath); tf != "" {
			full = append([]string{args[0], "--tf-path", tf}, args[1:]...)
		}
	}
	cmd := exec.CommandContext(ctx, opts.Binary, full...)
	cmd.Dir = opts.Dir
	cmd.Env = os.Environ()
	// Ctrl-C should let terragrunt and terraform unwind (and release their
	// state locks) rather than being killed mid-write.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	return cmd
}

// Discover lists the units the run would cover, without running anything.
func Discover(ctx context.Context, opts Options) ([]string, error) {
	args := []string{"find", "--format", "json", "--no-hidden"}
	if opts.Command != "" {
		args = append(args, "--queue-construct-as", opts.Command)
	}
	args = append(args, opts.filterArgs()...)
	out, err := output(ctx, opts, args)
	if err != nil {
		return nil, err
	}
	var found []struct {
		Type string `json:"type"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(out, &found); err != nil {
		return nil, err
	}
	units := make([]string, 0, len(found))
	for _, f := range found {
		if f.Type == "unit" {
			units = append(units, f.Path)
		}
	}
	return units, nil
}

// output runs a terragrunt command and returns its stdout.
func output(ctx context.Context, opts Options, args []string) ([]byte, error) {
	cmd := newCmd(ctx, opts, args)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", textutil.Headline(msg))
	}
	return out, nil
}

// stream runs a terragrunt command with JSON logging and feeds the progress
// display, recording errors as they happen.
func stream(ctx context.Context, opts Options, res *Result, args []string) error {
	args = append([]string{args[0], "--log-format", "json"}, args[1:]...)
	return streamCmd(ctx, opts, res, newCmd(ctx, opts, args), false)
}

// streamCmd runs a command and feeds its output to the progress display.
// direct says the output is terraform's own -json event stream rather than
// terragrunt log records.
func streamCmd(ctx context.Context, opts Options, res *Result, cmd *exec.Cmd, direct bool) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", opts.Binary, err)
	}

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
			if direct {
				mu.Lock()
				handled := handleTFEvent(line, opts, res)
				mu.Unlock()
				if !handled && opts.Progress != nil {
					opts.Progress.Raw(line)
				}
				continue
			}
			var ll LogLine
			if err := json.Unmarshal([]byte(line), &ll); err != nil || ll.Level == "" {
				// Not a terragrunt log record. terraform's own -json events
				// arrive here verbatim when a single unit runs.
				mu.Lock()
				if handleTFEvent(line, opts, res) {
					mu.Unlock()
					continue
				}
				mu.Unlock()
				if opts.Progress != nil {
					opts.Progress.Raw(line)
				}
				continue
			}
			mu.Lock()
			if res.TFPath == "" && ll.TFPath != "" {
				res.TFPath = ll.TFPath
			}
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
				// The wall of terraform text we exist to replace — but the
				// lines announcing what is being created right now are worth
				// showing while it happens.
				if opts.Progress != nil {
					opts.Progress.Unit(ll.WorkingDir)
					if unitFinished(ll.Msg) {
						opts.Progress.UnitDone(ll.WorkingDir)
					}
					if opts.Progress.Activity != nil {
						for _, l := range strings.Split(ll.Msg, "\n") {
							opts.Progress.Activity.Observe(ll.WorkingDir, l)
						}
					}
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
	wg.Wait()

	waitErr := cmd.Wait()
	var ee *exec.ExitError
	if waitErr != nil {
		if errors.As(waitErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			return fmt.Errorf("running %s: %w", opts.Binary, waitErr)
		}
	}
	return nil
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
// ones that ran, plus — for stack runs only — synthetic units for the ones
// that failed before writing a plan file.
//
// A single-unit run must not synthesize: terragrunt names that unit by its
// directory alone ("b") while the plan is filed under its project-relative
// path ("envs/prod/b"), and treating those as two units double-counts one.
func applyReport(run *model.Run, reportFile string, errs []string, synthesize bool) {
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
		if base := path.Base(run.Units[i].Path); base != run.Units[i].Path {
			byPath[base] = &run.Units[i]
		}
	}
	for _, e := range entries {
		name := filepath.ToSlash(e.Name)
		u, ok := byPath[name]
		if !ok && !synthesize {
			if len(run.Units) == 1 {
				u, ok = &run.Units[0], true
			} else {
				continue
			}
		}
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
			if len(u.Errors) == 0 {
				u.Errors = allErrorsFor(name, errs)
			}
		}
		if s, err1 := time.Parse(time.RFC3339Nano, e.Started); err1 == nil {
			if t, err2 := time.Parse(time.RFC3339Nano, e.Ended); err2 == nil {
				u.Duration = t.Sub(s)
			}
		}
	}
}

// allErrorsFor collects every diagnostic naming a unit. A single failure can
// repeat once per resource, and the count is part of the news.
func allErrorsFor(unit string, errs []string) []string {
	var out []string
	for _, e := range errs {
		for _, part := range textutil.SplitErrors(e) {
			if strings.Contains(part, unit) || len(errs) == 1 {
				out = append(out, part)
			}
		}
	}
	return out
}

// markInterrupted relabels units that were cut short by Ctrl-C. A unit that
// failed for a real reason carries terraform's diagnostic; one that was merely
// killed mid-flight carries only the placeholder, and calling that "failed"
// would send people hunting for a bug that is not there.
func markInterrupted(run *model.Run) {
	for i := range run.Units {
		u := &run.Units[i]
		if u.Errored && u.Error == failedPlaceholder {
			u.Errored = false
			u.Skipped = true
			u.Error = "interrupted"
		}
	}
}

// formatDiagnostic renders one terraform diagnostic with the context that
// makes it actionable: what went wrong, where, and the line it went wrong on.
func formatDiagnostic(ev TFEvent) string {
	d := ev.Diagnostic
	head := "Error: " + strings.TrimSpace(d.Summary)
	if d.Range.Filename != "" {
		head += fmt.Sprintf(" at %s:%d", d.Range.Filename, d.Range.Start.Line)
	}
	if detail := strings.TrimSpace(d.Detail); detail != "" {
		head += ": " + detail
	}

	lines := []string{head}
	if ctx := strings.TrimSpace(d.Snippet.Context); ctx != "" {
		lines = append(lines, "  in "+ctx)
	}
	if code := strings.TrimSpace(d.Snippet.Code); code != "" {
		line := d.Snippet.StartLine
		if line == 0 {
			line = d.Range.Start.Line
		}
		lines = append(lines, fmt.Sprintf("  %d: %s", line, code))
	}
	for _, v := range d.Snippet.Values {
		if v.Traversal != "" {
			lines = append(lines, "  "+strings.TrimSpace(v.Traversal+" "+v.Statement))
		}
	}
	return strings.Join(lines, "\n")
}

// unitFinished spots terraform announcing that it is done with a unit. There
// is no structured signal for this in a stack run: the report is only written
// at the very end.
func unitFinished(msg string) bool {
	return strings.Contains(msg, "Apply complete!") ||
		strings.Contains(msg, "Destroy complete!") ||
		strings.Contains(msg, "No changes. Infrastructure is up-to-date")
}

// handleTFEvent feeds terraform's own progress events into the display and
// reports whether the line was one. Caller holds the lock.
func handleTFEvent(line string, opts Options, res *Result) bool {
	var ev TFEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Type == "" {
		return false
	}
	if opts.Progress != nil && opts.Progress.Activity != nil {
		addr := ev.Hook.Resource.Addr
		if addr == "" {
			addr = ev.Change.Resource.Addr
		}
		action := ev.Hook.Action
		if action == "" {
			action = ev.Change.Action
		}
		opts.Progress.Activity.Event("", ev.Type, addr, action)
	}
	switch ev.Type {
	case "refresh_complete":
		if opts.Progress != nil {
			opts.Progress.Refreshed()
		}
	case "planned_change":
		if opts.Progress != nil {
			opts.Progress.PlannedResource()
		}
	case "apply_complete":
		if opts.Progress != nil {
			opts.Progress.PlannedResource()
		}
	case "diagnostic":
		if ev.Diagnostic.Severity == "error" {
			msg := formatDiagnostic(ev)
			res.Errors = append(res.Errors, msg)
			if opts.Progress != nil {
				opts.Progress.Error(textutil.Headline(msg))
			}
		}
	}
	return true
}

// firstErrorFor picks the error that belongs to one unit. terragrunt reports a
// stack failure as a single blob listing every unit, so the blob is split and
// only the part naming this unit is kept.
func firstErrorFor(unit string, errs []string) string {
	for _, e := range errs {
		for _, part := range textutil.SplitErrors(e) {
			if strings.Contains(part, unit) {
				return part
			}
		}
	}
	for _, e := range errs {
		if strings.Contains(e, unit) {
			return e
		}
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return failedPlaceholder
}

// failedPlaceholder is what a unit gets when terragrunt reported it as failed
// but produced no diagnostic of its own.
const failedPlaceholder = "failed"

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
