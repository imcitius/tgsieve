package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/imcitius/tgsieve/internal/render"
	"github.com/imcitius/tgsieve/internal/runner"
	"github.com/imcitius/tgsieve/internal/sieve"
	"github.com/imcitius/tgsieve/internal/textutil"
)

// cmdApply plans, shows the sieved report, asks, and then applies the plan
// files it just showed — not a fresh plan made after the answer. Terragrunt
// honours saved plan files, so what gets applied is what was on screen even if
// the configuration changes in between.
func cmdApply(args []string) (int, error) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	var cf commonFlags
	cf.bind(fs)
	all := fs.Bool("all", false, "apply across the whole stack below the working directory (terragrunt --all)")
	fs.BoolVar(all, "a", false, "shorthand for --all")
	plans := fs.String("plans", "", "apply plans saved earlier instead of planning now")
	autoApprove := fs.Bool("auto-approve", false, "do not ask; required outside a terminal")
	force := fs.Bool("force", false, "with --plans: apply even though the working tree changed since")
	tfPath := fs.String("tf-path", "", "tofu/terraform binary terragrunt should call")
	binary := fs.String("binary", "terragrunt", "terragrunt binary")
	tgArgs := fs.String("tg-args", "", "extra terragrunt flags, space separated")
	var filters stringList
	fs.Var(&filters, "filter", "terragrunt filter query, repeatable (requires --all)")
	filterAffected := fs.Bool("filter-affected", false, "only units affected by changes between main and HEAD (requires --all)")
	parallelism := fs.Int("parallelism", 0, "max units terragrunt runs at once (requires --all)")
	lockWait := fs.Duration("lock-wait", 0, "how long to wait for a busy --plans directory (e.g. 2m)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "tgsieve apply [flags]\n\n"+
			"Plans, shows what would change, asks, then applies exactly those plans.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitToolError, err
	}
	if (len(filters) > 0 || *filterAffected) && !*all {
		return exitToolError, fmt.Errorf("--filter/--filter-affected select units from a stack: add --all")
	}
	if err := cf.checkEngine(); err != nil {
		return exitToolError, err
	}
	if err := cf.checkStackFlags(*all, filters, *filterAffected, *parallelism); err != nil {
		return exitToolError, err
	}
	if *parallelism > 0 && !*all {
		return exitToolError, fmt.Errorf("--parallelism paces a stack run: add --all")
	}

	if err := cf.checkFormat(); err != nil {
		return exitToolError, err
	}
	cfg, err := cf.loadConfig()
	if err != nil {
		return exitToolError, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	color := useColor(cf.noColor)
	prog := runner.NewProgress(os.Stderr, isTTY(os.Stderr), cf.verbose)
	prog.Color = color

	opts := runner.Options{
		Dir:            cf.dir,
		Binary:         *binary,
		All:            *all,
		TerragruntArgs: strings.Fields(*tgArgs),
		TFPath:         *tfPath,
		Filters:        filters,
		FilterAffected: *filterAffected,
		Parallelism:    *parallelism,
		Engine:         cf.engineName(),
		Init:           cf.initFirst,
		Progress:       prog,
	}

	planDir := *plans
	reused := planDir != ""
	if planDir == "" {
		d, err := os.MkdirTemp("", "tgsieve-apply-")
		if err != nil {
			return exitToolError, err
		}
		defer os.RemoveAll(d)
		planDir = d
	}

	release, err := runner.LockWait(ctx, planDir, *lockWait)
	if err != nil {
		return exitToolError, err
	}
	defer release()

	now := runner.CurrentProvenance(ctx, cf.dir, "apply", version)

	// Phase one: get plans to look at.
	if reused {
		// Plans made against different code describe a world that no longer
		// exists; applying them is the one mistake this command must not make
		// quietly.
		if err := checkGeneration(planDir, now, *force, "drop --plans to plan and apply in one go"); err != nil {
			return exitToolError, err
		}
	} else {
		planOpts := opts
		planOpts.Command = "plan"
		planOpts.JSONOutDir = planDir
		planOpts.OutDir = planDir
		if _, err := runner.Run(ctx, planOpts); err != nil {
			return exitToolError, err
		}
	}

	run, err := runner.Collect(planDir)
	if err != nil {
		return exitToolError, err
	}
	runner.ApplyTimings(planDir, &run)
	rep := sieve.Apply(run, cfg)
	cf.render(os.Stdout, rep)

	if len(rep.ErroredUnits) > 0 {
		fmt.Fprintln(os.Stderr, "not applying: some units failed to plan")
		return exitUnitsFail, nil
	}
	if !rep.HasChanges() {
		fmt.Fprintln(os.Stderr, "nothing to apply")
		return exitOK, nil
	}

	// Phase two: ask.
	ok, err := approve(os.Stdin, os.Stderr, rep, *autoApprove, isTTY(os.Stdin))
	if err != nil {
		return exitToolError, err
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "aborted — nothing was applied")
		return exitAborted, nil
	}

	// Phase three: apply the plans that were on screen.
	applyProg := runner.NewProgress(os.Stderr, isTTY(os.Stderr), cf.verbose)
	applyProg.Color = color
	applyProg.Verb = "applying"
	applyProg.Track(rep.ChangedUnits())

	applyOpts := opts
	applyOpts.Command = "apply"
	applyOpts.OutDir = planDir
	applyOpts.Progress = applyProg
	res, err := runner.Run(ctx, applyOpts)
	if err != nil {
		return exitToolError, err
	}

	outcome := sieve.Apply(res.Run, cfg)
	outcome.Wall = res.Duration
	outcome.TFPath = res.TFPath
	outcome.Direct = cf.direct()
	renderOutcome(os.Stdout, rep, outcome, res, cf.renderOpts())

	if res.Interrupted {
		fmt.Fprintln(os.Stderr, "interrupted — some units may have applied")
		return exitInterrupt, nil
	}
	if len(outcome.ErroredUnits) > 0 || res.ExitCode != 0 {
		return exitUnitsFail, nil
	}
	return exitOK, nil
}

// approve asks before changing anything, twice when the plan destroys or
// replaces: those are the changes that cannot be undone by running the tool
// again.
func approve(in io.Reader, out io.Writer, rep *sieve.Report, autoApprove, interactive bool) (bool, error) {
	destructive := rep.Kept.Delete + rep.Kept.Replace
	if autoApprove {
		if destructive > 0 {
			fmt.Fprintf(out, "--auto-approve: applying %s, including %s that will be destroyed or replaced\n",
				plural(rep.Kept.Total(), "change"), plural(destructive, "resource"))
		}
		return true, nil
	}
	if !interactive {
		return false, fmt.Errorf("refusing to apply without a terminal to ask: pass --auto-approve if that is what you mean")
	}

	reader := bufio.NewReader(in)
	fmt.Fprintf(out, "\napply %s across %s? [yes/no] ",
		plural(rep.Kept.Total(), "change"), plural(rep.UnitsChanged, "unit"))
	answer, err := reader.ReadString('\n')
	if err != nil && answer == "" {
		return false, nil
	}
	if strings.TrimSpace(strings.ToLower(answer)) != "yes" {
		return false, nil
	}

	if destructive == 0 {
		return true, nil
	}
	fmt.Fprintf(out, "%s will be destroyed or replaced — type 'destroy' to confirm: ",
		plural(destructive, "resource"))
	answer, err = reader.ReadString('\n')
	if err != nil && answer == "" {
		return false, nil
	}
	return strings.TrimSpace(strings.ToLower(answer)) == "destroy", nil
}

// applyFailed reports whether the apply ran to completion. Anything short of
// that must not be described as applied: a report claiming changes that never
// happened is worse than no report.
func applyFailed(outcome *sieve.Report, res *runner.Result) bool {
	return res.Interrupted || res.ExitCode != 0 || len(outcome.ErroredUnits) > 0
}

// renderOutcome reports what the apply did with the plans that were shown.
func renderOutcome(w io.Writer, planned, outcome *sieve.Report, res *runner.Result, opts render.Options) {
	p := paletteFor(opts.Color)
	took := res.Duration.Round(100 * time.Millisecond)

	if applyFailed(outcome, res) {
		headline := "APPLY FAILED"
		if res.Interrupted {
			headline = "APPLY INTERRUPTED"
		}
		fmt.Fprintf(w, "\n%s\n", p("1;31", headline))
		fmt.Fprintf(w, "  %s\n", p("2", fmt.Sprintf(
			"stopped after %s — the report above is what was planned, not what landed", took)))

		switch {
		case len(outcome.Failures) > 0:
			for _, g := range outcome.Failures {
				fmt.Fprintf(w, "  %s %s\n", p("31", "✗"), strings.Join(g.Units, ", "))
				for _, line := range g.Detail {
					fmt.Fprintf(w, "      %s\n", p("2", line))
				}
			}
		case len(res.Errors) > 0:
			for _, e := range res.Errors {
				for _, line := range textutil.CleanError(e, 6) {
					fmt.Fprintf(w, "  %s %s\n", p("31", "✗"), line)
				}
			}
		}
		if res.TFPath != "" {
			fmt.Fprintf(w, "  %s\n", p("2", "terragrunt ran "+res.TFPath))
		}
		fmt.Fprintf(w, "  %s\n", p("2", "run tgsieve plan to see where things actually stand"))
		return
	}

	fmt.Fprintf(w, "\n%s\n", p("1", "APPLIED"))
	fmt.Fprintf(w, "  %s in %s\n",
		p("2", fmt.Sprintf("%s across %s", plural(planned.Kept.Total(), "change"),
			plural(planned.UnitsChanged, "unit"))), took)
	if c := planned.Kept; c.Delete+c.Replace > 0 {
		fmt.Fprintf(w, "  %s\n", p("2", fmt.Sprintf("%d destroyed, %d replaced", c.Delete, c.Replace)))
	}
}

// paletteFor keeps the outcome block's colouring in step with the renderer
// without exporting its painter.
func paletteFor(on bool) func(code, s string) string {
	return func(code, s string) string {
		if !on {
			return s
		}
		return "\033[" + code + "m" + s + "\033[0m"
	}
}
