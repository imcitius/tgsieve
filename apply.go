package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
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
	prog.Window = runner.WindowSize(cf.window)
	prog.Activity = runner.NewActivity()

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
	rep.Direct = cf.direct()
	// A machine-readable apply emits one document at the end describing both
	// what was planned and what happened; printing the plan first would leave
	// consumers with two documents on one stream.
	if !cf.jsonFormat() {
		cf.render(os.Stdout, rep, cf.meta("apply"))
	}

	if len(rep.ErroredUnits) > 0 {
		if cf.jsonFormat() {
			cf.render(os.Stdout, rep, cf.meta("apply"))
		}
		fmt.Fprintln(os.Stderr, "not applying: some units failed to plan")
		return exitUnitsFail, nil
	}
	if !rep.HasChanges() {
		if cf.jsonFormat() {
			cf.render(os.Stdout, rep, cf.meta("apply"))
		}
		if rep.Kept.Drift > 0 {
			// Drift is state catching up with reality, not work an apply does.
			fmt.Fprintf(os.Stderr,
				"nothing to apply — the %s above %s already been recorded by the refresh, and this plan does not act on %s\n",
				plural(rep.Kept.Drift, "drifted resource"), was(rep.Kept.Drift), them(rep.Kept.Drift))
			return exitOK, nil
		}
		fmt.Fprintln(os.Stderr, "nothing to apply")
		return exitOK, nil
	}

	// Phase two: ask.
	ok, err := approve(ctx, os.Stdin, os.Stderr, rep, *autoApprove, isTTY(os.Stdin))
	if err != nil {
		return exitToolError, err
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\ninterrupted — nothing was applied")
		return exitInterrupt, nil
	}
	if !ok {
		if cf.jsonFormat() {
			cf.render(os.Stdout, rep, cf.meta("apply"))
		}
		fmt.Fprintln(os.Stderr, "aborted — nothing was applied")
		return exitAborted, nil
	}

	// Phase three: apply the plans that were on screen.
	applyProg := runner.NewProgress(os.Stderr, isTTY(os.Stderr), cf.verbose)
	applyProg.Color = color
	applyProg.Verb = "applying"
	applyProg.Window = runner.WindowSize(cf.window)
	applyProg.Activity = runner.NewActivity()
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

	done, unfinished := applyProg.Activity.Results()

	switch {
	case cf.jsonFormat():
		applied := render.AppliedResult(!applyFailed(outcome, res),
			res.Duration.Milliseconds(), rep.ChangedUnits(), res.Errors)
		if err := render.JSON(os.Stdout, rep, cf.meta("apply"), applied); err != nil {
			fmt.Fprintf(os.Stderr, "tgsieve: writing json: %v\n", err)
		}
	case cf.markdown():
		renderOutcomeMarkdown(os.Stdout, rep, outcome, res)
	default:
		renderOutcome(os.Stdout, rep, outcome, res, done, unfinished, cf.renderOpts())
	}

	if res.Interrupted {
		fmt.Fprintln(os.Stderr, "interrupted — some units may have applied")
		return exitInterrupt, nil
	}
	if len(outcome.ErroredUnits) > 0 || res.ExitCode != 0 {
		return exitUnitsFail, nil
	}
	return exitOK, nil
}

func was(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// approve asks before changing anything, twice when the plan destroys or
// replaces: those are the changes that cannot be undone by running the tool
// again.
func approve(ctx context.Context, in io.Reader, out io.Writer, rep *sieve.Report, autoApprove, interactive bool) (bool, error) {
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
	answer, ok := readLine(ctx, reader)
	if !ok || strings.TrimSpace(strings.ToLower(answer)) != "yes" {
		return false, nil
	}

	if destructive == 0 {
		return true, nil
	}
	fmt.Fprintf(out, "%s will be destroyed or replaced — type 'destroy' to confirm: ",
		plural(destructive, "resource"))
	answer, ok = readLine(ctx, reader)
	if !ok {
		return false, nil
	}
	return strings.TrimSpace(strings.ToLower(answer)) == "destroy", nil
}

// readLine waits for an answer without ignoring Ctrl-C. A plain blocking read
// swallows the interrupt: the signal is handled, the read keeps waiting, and
// the terminal echoes "^C" as if the keypress did nothing.
func readLine(ctx context.Context, r *bufio.Reader) (string, bool) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", false
	case res := <-ch:
		if res.err != nil && res.line == "" {
			return "", false
		}
		return res.line, true
	}
}

// applyFailed reports whether the apply ran to completion. Anything short of
// that must not be described as applied: a report claiming changes that never
// happened is worse than no report.
func applyFailed(outcome *sieve.Report, res *runner.Result) bool {
	return res.Interrupted || res.ExitCode != 0 || len(outcome.ErroredUnits) > 0
}

// renderOutcome reports what the apply did with the plans that were shown.
func renderOutcome(w io.Writer, planned, outcome *sieve.Report, res *runner.Result, done, unfinished []runner.Outcome, opts render.Options) {
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
		renderLanded(w, res, done, unfinished, opts, true)
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
	renderLanded(w, res, done, unfinished, opts, false)
}

// renderOutcomeMarkdown reports the apply in the same document shape as the
// plan above it, so a comment reads as one story.
func renderOutcomeMarkdown(w io.Writer, planned, outcome *sieve.Report, res *runner.Result) {
	took := res.Duration.Round(100 * time.Millisecond)
	if applyFailed(outcome, res) {
		headline := "### Apply failed"
		if res.Interrupted {
			headline = "### Apply interrupted"
		}
		fmt.Fprintf(w, "\n%s\n\n", headline)
		fmt.Fprintf(w, "Stopped after %s — the report above is what was planned, not what landed.\n", took)
		switch {
		case len(outcome.Failures) > 0:
			for _, g := range outcome.Failures {
				fmt.Fprintf(w, "\n**%s**\n\n```\n%s\n```\n", strings.Join(g.Units, ", "), strings.Join(g.Detail, "\n"))
			}
		case len(res.Errors) > 0:
			fmt.Fprintf(w, "\n```\n%s\n```\n", strings.Join(res.Errors, "\n"))
		}
		fmt.Fprint(w, "\nRun `tgsieve plan` to see where things actually stand.\n")
		return
	}
	fmt.Fprintf(w, "\n### Applied\n\n%s across %s in %s.\n",
		plural(planned.Kept.Total(), "change"), plural(planned.UnitsChanged, "unit"), took)
	if c := planned.Kept; c.Delete+c.Replace > 0 {
		fmt.Fprintf(w, "\n%d destroyed, %d replaced.\n", c.Delete, c.Replace)
	}
}

// renderLanded reports what the apply actually did to infrastructure, which
// after a failure is the question the plan above cannot answer: it describes
// intent, and the error says why it stopped, but neither says how far it got.
func renderLanded(w io.Writer, res *runner.Result, done, unfinished []runner.Outcome, opts render.Options, failed bool) {
	p := paletteFor(opts.Color)
	if len(done) == 0 && len(unfinished) == 0 {
		return
	}

	// Counted from what terraform reported doing, not from the plan: the sieve
	// may have hidden resources from the report, and comparing the two would
	// produce arithmetic like "21 of 20".
	parts := []string{"terraform changed " + plural(len(done), "resource")}
	if len(unfinished) > 0 {
		parts = append(parts, fmt.Sprintf("%d did not finish", len(unfinished)))
	}
	if len(done) > 1 {
		parts = append(parts, "slowest first")
	}
	fmt.Fprintf(w, "  %s\n", p("2", strings.Join(parts, " · ")))

	sort.SliceStable(done, func(i, j int) bool { return done[i].Took > done[j].Took })

	for _, o := range unfinished {
		fmt.Fprintf(w, "    %s %s%s\n", p("31", "✗"), scopePrefix(o.Unit)+o.Addr,
			p("31", fmt.Sprintf(" — %s, did not finish after %s", o.Verb, o.Took.Round(time.Second))))
	}
	// After a failure this list is the evidence of what landed, so it is worth
	// more room than the summary of a run that went fine.
	limit := opts.MaxUnits
	if limit <= 0 {
		limit = 6
	}
	if failed && limit < 20 {
		limit = 20
	}
	shown := done
	extra := 0
	if len(shown) > limit {
		extra = len(shown) - limit
		shown = shown[:limit]
	}
	for _, o := range shown {
		fmt.Fprintf(w, "    %s %s %s\n", p("32", "✓"), scopePrefix(o.Unit)+o.Addr,
			p("2", fmt.Sprintf("%s in %s", runner.Past(o.Verb), o.Took.Round(time.Second))))
	}
	if extra > 0 {
		fmt.Fprintf(w, "    %s\n", p("2", fmt.Sprintf("… and %d more applied", extra)))
	}

	if len(res.Outputs) > 0 {
		fmt.Fprintf(w, "  %s\n", p("1", "OUTPUTS"))
		names := make([]string, 0, len(res.Outputs))
		for k := range res.Outputs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(w, "    %s %s\n", k, p("2", outputValue(res.Outputs[k], opts.MaxValue)))
		}
	}
}

// scopePrefix names the unit when there is one to name. A single-module run
// has no unit worth repeating on every line.
func scopePrefix(unit string) string {
	if unit == "" {
		return ""
	}
	return unit + " "
}

// outputValue renders an output, keeping sensitive ones out of the terminal.
func outputValue(v any, max int) string {
	m, ok := v.(map[string]any)
	if !ok {
		return render.FormatValue(v, max)
	}
	if s, _ := m["sensitive"].(bool); s {
		return "(sensitive)"
	}
	return render.FormatValue(m["value"], max)
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
