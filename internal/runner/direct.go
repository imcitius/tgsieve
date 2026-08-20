package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/imcitius/tgsieve/internal/model"
)

// EngineTerraform drives terraform (or tofu) directly, with no terragrunt in
// the picture. The sieve never cared where a plan document came from: a root
// module big enough to be unreadable is the same problem as a stack, minus the
// queue.
const EngineTerraform = "terraform"

// Direct reports whether this run bypasses terragrunt.
func (o Options) Direct() bool { return o.Engine == EngineTerraform }

// directBinary picks the binary for an engine-less run: what the caller asked
// for, else terraform, else tofu.
func directBinary(opts Options) string {
	if opts.TFPath != "" {
		return opts.TFPath
	}
	if p := os.Getenv("TG_TF_PATH"); p != "" {
		return p
	}
	for _, candidate := range []string{"terraform", "tofu"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	return "terraform"
}

// runDirect plans or applies with terraform itself, over the root modules in
// play: the working directory, or every directory --unit named.
func runDirect(ctx context.Context, opts Options, planDir string, res *Result) error {
	dirs := opts.Dirs
	if len(dirs) == 0 {
		dirs = []string{opts.Dir}
	}
	worst := 0
	for _, dir := range dirs {
		one := opts
		one.Dir = dir
		one.Dirs = nil
		if len(dirs) > 1 {
			// With one module the report has nowhere else to put a resource;
			// with several, saying which one it belongs to is the point.
			one.unit = unitName(dir)
		}
		if err := runDirectOne(ctx, one, planDir, res); err != nil {
			return err
		}
		if res.ExitCode > worst {
			worst = res.ExitCode
		}
		if ctx.Err() != nil {
			break
		}
	}
	// One module that failed fails the run, however many succeeded after it.
	res.ExitCode = worst
	return nil
}

func runDirectOne(ctx context.Context, opts Options, planDir string, res *Result) error {
	bin := directBinary(opts)
	res.TFPath = bin
	unit := unitName(opts.Dir)
	started := time.Now()
	// terraform names no module in its diagnostics, so this one owns whatever
	// it adds to the list while it runs.
	before := len(res.Errors)
	mine := func() []string { return append([]string(nil), res.Errors[before:]...) }

	if opts.Init {
		if err := directCmd(ctx, opts, bin, res, []string{"init", "-input=false"}); err != nil {
			return err
		}
		if res.ExitCode != 0 {
			res.directFailures = append(res.directFailures, failedUnit(unit, started, mine()))
			return nil
		}
	}

	planFile, cleanup, err := directPlanPath(opts, unit)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	if opts.Command == "apply" {
		if _, err := os.Stat(planFile); err != nil {
			return fmt.Errorf("no saved plan at %s", planFile)
		}
		if err := directCmd(ctx, opts, bin, res, []string{"apply", "-input=false", "-json", planFile}); err != nil {
			return err
		}
		u := model.Unit{Path: unit, Duration: time.Since(started), Errored: res.ExitCode != 0}
		if u.Errored {
			// Without this the report says only "failed", while the reason
			// terraform gave scrolls past in the live feed and is lost.
			errs := mine()
			u.Error = firstErrorFor(unit, errs)
			u.Errors = errs
		}
		res.Run.Units = append(res.Run.Units, u)
		return nil
	}

	args := []string{"plan", "-input=false", "-json", "-out=" + planFile}
	args = append(args, opts.TFArgs...)
	if err := directCmd(ctx, opts, bin, res, args); err != nil {
		return err
	}
	if res.ExitCode != 0 {
		res.directFailures = append(res.directFailures, failedUnit(unit, started, mine()))
		return nil
	}

	// The plan file is binary; the report needs the JSON document terraform
	// renders from it, which is the same shape terragrunt's --json-out-dir
	// writes for a stack.
	out, err := directOutput(ctx, opts, bin, []string{"show", "-json", planFile})
	if err != nil {
		return fmt.Errorf("rendering the plan as JSON: %w", err)
	}
	unitDir := filepath.Join(planDir, unit)
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(unitDir, planFileName), out, 0o644); err != nil {
		return err
	}
	if res.durations == nil {
		res.durations = map[string]time.Duration{}
	}
	res.durations[unit] = time.Since(started)
	return nil
}

// failedUnit records a root module that never produced a plan, with the
// diagnostics it produced instead.
func failedUnit(unit string, started time.Time, errs []string) model.Unit {
	return model.Unit{
		Path:     unit,
		Duration: time.Since(started),
		Errored:  true,
		Error:    firstErrorFor(unit, errs),
		Errors:   errs,
	}
}

// directPlanPath decides where the binary plan lives, keeping the same layout
// as a stack run so --plans works the same way in both engines.
func directPlanPath(opts Options, unit string) (string, func(), error) {
	if opts.OutDir != "" {
		abs, err := filepath.Abs(filepath.Join(opts.OutDir, unit))
		if err != nil {
			return "", nil, err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", nil, err
		}
		return filepath.Join(abs, "tfplan.tfplan"), nil, nil
	}
	f, err := os.CreateTemp("", "tgsieve-*.tfplan")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	f.Close()
	return name, func() { os.Remove(name) }, nil
}

// directCmd runs terraform and feeds its -json event stream to the progress
// display, collecting diagnostics as errors.
func directCmd(ctx context.Context, opts Options, bin string, res *Result, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = opts.Dir
	cmd.Env = os.Environ()
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	return streamCmd(ctx, opts, res, cmd, true)
}

func directOutput(ctx context.Context, opts Options, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = opts.Dir
	cmd.Env = os.Environ()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return out, nil
}
