// tgsieve runs terragrunt and shows only the changes that matter.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/imcitius/tgsieve/internal/config"
	"github.com/imcitius/tgsieve/internal/render"
	"github.com/imcitius/tgsieve/internal/runner"
	"github.com/imcitius/tgsieve/internal/sieve"
)

// Overridden at release time via -ldflags -X main.version=… (see .goreleaser.yaml).
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// Exit codes. A caller has to be able to tell a stack that failed from a tool
// that broke, and neither from a plan that simply has changes in it.
const (
	exitOK        = 0
	exitToolError = 1 // tgsieve itself could not do its job
	exitChanges   = 2 // --detailed-exitcode: changes survived the sieve
	exitUnitsFail = 3 // one or more units failed to plan
	exitInterrupt = 130
)

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func versionString() string {
	s := "tgsieve " + version
	if commit != "" {
		s += " (" + commit
		if date != "" {
			s += ", " + date
		}
		s += ")"
	}
	return s
}

const usage = `tgsieve — terragrunt plans without the wall of text

Usage:
  tgsieve plan [flags] [-- <tofu/terraform args>]   run a plan and show real changes
  tgsieve show <plan-dir> [flags]                   re-render plans saved earlier
  tgsieve rules [flags]                             print the effective sieve config
  tgsieve presets [name]                            list the built-in rule sets, or show one
  tgsieve init                                      write a starter .tgsieve.yaml at the project root
  tgsieve version

The stack is never planned implicitly: without --all only the unit in the
working directory runs.

Exit codes: 0 fine · 1 tgsieve failed · 2 changes survived the sieve
(--detailed-exitcode) · 3 a unit failed to plan · 130 interrupted.

Examples:
  tgsieve plan                                  # this unit only
  tgsieve plan --all                            # the whole stack below here
  tgsieve plan --all --filter-affected          # only what changed vs main
  tgsieve plan --all --timings                  # with the slowest units
  tgsieve plan -C envs/prod --all -- -refresh=false
  tgsieve plan --all --keep-plans ./plans --out-dir ./tfplans
  tgsieve show ./plans --explain
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	var code int
	switch os.Args[1] {
	case "plan":
		code, err = cmdPlan(os.Args[2:])
	case "show":
		code, err = cmdShow(os.Args[2:])
	case "rules":
		err = cmdRules(os.Args[2:])
	case "presets":
		err = cmdPresets(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(versionString())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tgsieve: %v\n", err)
		os.Exit(exitToolError)
	}
	os.Exit(code)
}

// stringList collects a flag that may be repeated.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

type commonFlags struct {
	dir        string
	configPath string
	verbose    bool
	showEmpty  bool
	explain    bool
	noSieve    bool
	noColor    bool
	timings    bool
	maxAttrs   int
	maxUnits   int
}

func (c *commonFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.dir, "C", ".", "working directory")
	fs.StringVar(&c.configPath, "config", "", "path to .tgsieve.yaml (default: nearest, walking up)")
	fs.BoolVar(&c.verbose, "v", false, "show attributes for creates and deletes too")
	fs.BoolVar(&c.showEmpty, "show-empty", false, "list units with no changes")
	fs.BoolVar(&c.explain, "explain", false, "show every attribute the sieve hid, and which rule hid it")
	fs.BoolVar(&c.noSieve, "no-sieve", false, "disable noise rules (still collapses duplicates)")
	fs.BoolVar(&c.noColor, "no-color", false, "disable color")
	fs.BoolVar(&c.timings, "timings", false, "list the slowest units")
	fs.IntVar(&c.maxAttrs, "max-attrs", 12, "max attributes shown per resource")
	fs.IntVar(&c.maxUnits, "max-units", 6, "max unit names listed per collapsed group")
}

func (c *commonFlags) renderOpts() render.Options {
	return render.Options{
		Color:     useColor(c.noColor),
		Verbose:   c.verbose,
		ShowEmpty: c.showEmpty,
		Explain:   c.explain,
		Timings:   c.timings,
		MaxAttrs:  c.maxAttrs,
		MaxUnits:  c.maxUnits,
	}
}

func (c *commonFlags) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(c.dir, c.configPath)
	if err != nil {
		return nil, err
	}
	if c.noSieve {
		cfg.Ignore = nil
	}
	return cfg, nil
}

func cmdPlan(args []string) (int, error) {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	var cf commonFlags
	cf.bind(fs)
	// Opt-in, never implied: a wrapper that quietly widens the blast radius is
	// how a stack-wide run happens by accident.
	all := fs.Bool("all", false, "run across the whole stack below the working directory (terragrunt --all)")
	fs.BoolVar(all, "a", false, "shorthand for --all")
	keepPlans := fs.String("keep-plans", "", "keep per-unit tfplan.json in this directory")
	outDir := fs.String("out-dir", "", "save binary plan files here (apply exactly what you reviewed)")
	tfPath := fs.String("tf-path", "", "tofu/terraform binary terragrunt should call")
	binary := fs.String("binary", "terragrunt", "terragrunt binary")
	detailed := fs.Bool("detailed-exitcode", false, "exit 2 when changes survive the sieve, 0 when none")
	failOn := fs.String("fail-on", "", "exit 2 when a surviving change is this severe or worse: low|medium|high")
	tgArgs := fs.String("tg-args", "", "extra terragrunt flags, space separated")
	var filters stringList
	fs.Var(&filters, "filter", "terragrunt filter query, repeatable (requires --all)")
	filterAffected := fs.Bool("filter-affected", false, "only units affected by changes between main and HEAD (requires --all)")
	parallelism := fs.Int("parallelism", 0, "max units terragrunt runs at once (requires --all)")
	fast := fs.Bool("fast", false, "skip the refresh (-refresh=false): much faster, but blind to out-of-band changes")
	noResolveRefs := fs.Bool("no-resolve-refs", false, "do not contact module remotes to pin floating refs (offline)")
	lockWait := fs.Duration("lock-wait", 0, "how long to wait for a busy --keep-plans directory (e.g. 2m)")
	resume := fs.Bool("resume", false, "only run units that have no plan in --keep-plans yet")
	force := fs.Bool("force", false, "with --resume: reuse plans even though the working tree changed since")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "tgsieve plan [flags] [-- <tofu/terraform args>]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitToolError, err
	}

	cfg, err := cf.loadConfig()
	if err != nil {
		return exitToolError, err
	}
	// Filters select from a queue, and there is no queue without --all. Saying
	// so beats silently widening the run to the whole stack.
	if (len(filters) > 0 || *filterAffected) && !*all {
		return exitToolError, fmt.Errorf("--filter/--filter-affected select units from a stack: add --all")
	}
	if *parallelism > 0 && !*all {
		return exitToolError, fmt.Errorf("--parallelism paces a stack run: add --all")
	}
	if *failOn != "" && config.SeverityRank(*failOn) == 0 {
		return exitToolError, fmt.Errorf("--fail-on: want low, medium or high, got %q", *failOn)
	}
	if *resume {
		if *keepPlans == "" {
			return exitToolError, fmt.Errorf("--resume needs --keep-plans <dir>: that is where the earlier plans are")
		}
		if !*all {
			return exitToolError, fmt.Errorf("--resume picks up the rest of a stack run: add --all")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	color := useColor(cf.noColor)
	prog := runner.NewProgress(os.Stderr, isTTY(os.Stderr), cf.verbose)
	prog.Color = color

	tfArgs := fs.Args()
	if *fast && !hasRefreshFlag(tfArgs) {
		tfArgs = append([]string{"-refresh=false"}, tfArgs...)
	}

	opts := runner.Options{
		Dir:            cf.dir,
		Binary:         *binary,
		All:            *all,
		Command:        "plan",
		TFArgs:         tfArgs,
		TerragruntArgs: strings.Fields(*tgArgs),
		JSONOutDir:     *keepPlans,
		OutDir:         *outDir,
		TFPath:         *tfPath,
		Filters:        filters,
		FilterAffected: *filterAffected,
		Parallelism:    *parallelism,
		NoResolveRefs:  *noResolveRefs,
		Progress:       prog,
	}

	if *keepPlans != "" {
		if *lockWait > 0 {
			fmt.Fprintf(os.Stderr, "waiting up to %s for %s\n", *lockWait, *keepPlans)
		}
		release, err := runner.LockWait(ctx, *keepPlans, *lockWait)
		if err != nil {
			return exitToolError, err
		}
		defer release()
	}

	now := runner.CurrentProvenance(ctx, cf.dir, "plan", version)
	if *keepPlans != "" {
		// Only worth the extra terragrunt calls when the plans are being kept:
		// nothing else compares generations.
		units := []string{"."}
		if *all {
			if found, err := runner.Discover(ctx, opts); err == nil {
				units = found
				opts.KnownUnits = found
			}
		}
		now.Sources = runner.ModuleSources(ctx, opts, units)
	}

	reused := 0
	if *resume {
		if err := checkGeneration(*keepPlans, now, *force); err != nil {
			return exitToolError, err
		}
		done, missing, err := resumeSplit(ctx, opts, *keepPlans)
		if err != nil {
			return exitToolError, err
		}
		reused = done
		if len(missing) == 0 {
			fmt.Fprintf(os.Stderr, "nothing to resume: all %d units already have plans in %s\n", reused, *keepPlans)
			return renderSaved(*keepPlans, cfg, cf, *detailed)
		}
		opts.Filters = missing
		opts.FilterAffected = false // already folded into the unit list
		fmt.Fprintf(os.Stderr, "resuming: %d units already planned, %d to run\n", reused, len(missing))
	}

	res, err := runner.Run(ctx, opts)
	if err != nil {
		return exitToolError, err
	}
	if *keepPlans != "" {
		if err := runner.SaveTimings(*keepPlans, res.Run); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record unit timings: %v\n", err)
		}
		runner.ApplyTimings(*keepPlans, &res.Run)
	}
	if *keepPlans != "" && !*resume {
		// Only a fresh run defines the generation; a resume adds to it.
		if err := runner.WriteProvenance(*keepPlans, now); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record plan provenance: %v\n", err)
		}
	}

	rep := sieve.Apply(res.Run, cfg)
	rep.Wall = res.Duration
	rep.NoRefresh = refreshDisabled(tfArgs)
	render.TTY(os.Stdout, rep, cf.renderOpts())
	if reused > 0 {
		fmt.Fprintf(os.Stderr, "  %d of the plans above were reused from a previous run\n", reused)
	}

	if res.Interrupted {
		// Whatever finished before Ctrl-C is still worth reading, so the
		// report is printed first and the interruption reported after it.
		fmt.Fprintln(os.Stderr, "interrupted — the report above covers the units that finished")
		return exitInterrupt, nil
	}
	if len(rep.ErroredUnits) > 0 {
		return exitUnitsFail, nil
	}
	if res.ExitCode != 0 && len(res.Run.Units) == 0 {
		// terragrunt failed before any unit produced a plan: nothing ran, so
		// this is a tooling or configuration problem, not a stack failure.
		return exitToolError, nil
	}
	if *failOn != "" && rep.AtLeast(*failOn) {
		return exitChanges, nil
	}
	if *detailed && rep.HasChanges() {
		return exitChanges, nil
	}
	return exitOK, nil
}

// hasRefreshFlag reports whether the caller already decided about refreshing.
func hasRefreshFlag(args []string) bool {
	for _, a := range args {
		if a == "-refresh" || a == "--refresh" || strings.HasPrefix(a, "-refresh=") || strings.HasPrefix(a, "--refresh=") {
			return true
		}
	}
	return false
}

// refreshDisabled reports whether this plan skipped the refresh, however that
// was asked for. The report says so, because a stale plan that looks clean is
// worse than a slow one.
func refreshDisabled(args []string) bool {
	for i, a := range args {
		switch {
		case a == "-refresh=false", a == "--refresh=false":
			return true
		case a == "-refresh", a == "--refresh":
			return i+1 < len(args) && args[i+1] == "false"
		}
	}
	return false
}

// checkGeneration refuses to mix plans from one state of the code with a run
// against another. A stale plan that still renders is the worst outcome here:
// it looks exactly like a fresh one.
func checkGeneration(dir string, now runner.Provenance, force bool) error {
	was, err := runner.ReadProvenance(dir)
	if err != nil {
		if force {
			fmt.Fprintf(os.Stderr, "warning: %v (continuing because --force)\n", err)
			return nil
		}
		return fmt.Errorf("%w\n  re-run without --resume, or pass --force to reuse them anyway", err)
	}
	if was.SameGeneration(now) {
		return nil
	}
	msg := fmt.Sprintf("the plans in %s were made at %s, the working tree is now at %s",
		dir, was.Describe(), now.Describe())
	if moved := was.SourceChanges(now); len(moved) > 0 {
		shown := moved
		if len(shown) > 3 {
			shown = shown[:3]
		}
		msg = fmt.Sprintf("the module source moved under %s since %s was written (%s)",
			plural(len(moved), "unit"), dir, strings.Join(shown, ", "))
	}
	if force {
		fmt.Fprintf(os.Stderr, "warning: %s (continuing because --force)\n", msg)
		return nil
	}
	return fmt.Errorf("%s\n  re-run without --resume to plan the stack fresh, or pass --force to mix generations", msg)
}

// resumeSplit compares the units the run would cover with the plans already
// sitting in dir, and returns how many are done plus the ones still missing.
func resumeSplit(ctx context.Context, opts runner.Options, dir string) (int, []string, error) {
	discovered, err := runner.Discover(ctx, opts)
	if err != nil {
		return 0, nil, fmt.Errorf("listing units to resume: %w", err)
	}
	saved, err := runner.Collect(dir)
	if err != nil {
		return 0, nil, err
	}
	have := make(map[string]bool, len(saved.Units))
	for _, u := range saved.Units {
		have[u.Path] = true
	}
	var missing []string
	done := 0
	for _, u := range discovered {
		if have[u] {
			done++
			continue
		}
		missing = append(missing, u)
	}
	return done, missing, nil
}

// renderSaved prints the report for plans already on disk, without running
// anything.
func renderSaved(dir string, cfg *config.Config, cf commonFlags, detailed bool) (int, error) {
	run, err := runner.Collect(dir)
	if err != nil {
		return exitToolError, err
	}
	runner.ApplyTimings(dir, &run)
	rep := sieve.Apply(run, cfg)
	render.TTY(os.Stdout, rep, cf.renderOpts())
	if detailed && rep.HasChanges() {
		return exitChanges, nil
	}
	return exitOK, nil
}

func cmdShow(args []string) (int, error) {
	if len(args) == 0 {
		return exitToolError, fmt.Errorf("show needs a plan directory (the one you passed to --keep-plans)")
	}
	dir := args[0]
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	var cf commonFlags
	cf.bind(fs)
	detailed := fs.Bool("detailed-exitcode", false, "exit 2 when changes survive the sieve")
	if err := fs.Parse(args[1:]); err != nil {
		return exitToolError, err
	}
	cfg, err := cf.loadConfig()
	if err != nil {
		return exitToolError, err
	}
	run, err := runner.Collect(dir)
	if err != nil {
		return exitToolError, err
	}
	runner.ApplyTimings(dir, &run)
	if len(run.Units) == 0 {
		return exitToolError, fmt.Errorf("no tfplan.json found under %s", dir)
	}
	rep := sieve.Apply(run, cfg)
	render.TTY(os.Stdout, rep, cf.renderOpts())
	if len(rep.ErroredUnits) > 0 {
		return exitUnitsFail, nil
	}
	if *detailed && rep.HasChanges() {
		return exitChanges, nil
	}
	return exitOK, nil
}

func cmdRules(args []string) error {
	fs := flag.NewFlagSet("rules", flag.ExitOnError)
	var cf commonFlags
	cf.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := cf.loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Sources) == 0 {
		fmt.Println("no .tgsieve.yaml found — using defaults (nothing is hidden)")
	} else {
		fmt.Println("config files (later wins):")
		for _, s := range cfg.Sources {
			fmt.Println("  " + s)
		}
	}
	fmt.Printf("\ncollapse: instances=%v cross_unit=%v mode=%s min_units=%d\n",
		deref(cfg.Collapse.Instances), deref(cfg.Collapse.CrossUnit), orDefault(cfg.Collapse.CrossUnitMode, "shape"), cfg.Collapse.MinUnits)
	fmt.Printf("never_hide: actions=%v types=%v\n", cfg.NeverHide.List(), cfg.NeverHide.Types)
	fmt.Printf("\nignore rules (%d):\n", len(cfg.Ignore))
	for i, r := range cfg.Ignore {
		fmt.Printf("  %s\n", r.Label(i))
	}
	return nil
}

func cmdPresets(args []string) error {
	if len(args) > 0 {
		rules, err := config.PresetRules(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("%s (%s)\n\n", args[0], plural(len(rules), "rule"))
		for i, r := range rules {
			fmt.Printf("  %s\n", r.Label(i))
		}
		return nil
	}
	fmt.Print("built-in rule sets — opt in with 'extends' in .tgsieve.yaml:\n\n")
	for _, name := range config.PresetNames() {
		rules, err := config.PresetRules(name)
		if err != nil {
			return err
		}
		attrs := 0
		for _, r := range rules {
			attrs += len(r.Attrs)
		}
		fmt.Printf("  %-28s %s over %s\n", name, plural(len(rules), "rule"), plural(attrs, "attribute"))
	}
	fmt.Print("\n  tgsieve presets <name>   show what one of them hides\n")
	return nil
}

const starterConfig = `# .tgsieve.yaml — what to treat as noise.
# Nothing is hidden until you say so. Run "tgsieve plan --explain" to see
# exactly what each rule swallowed.
version: 1

hide:
  unchanged_units: true   # units with nothing left to say collapse into a count
  drift: false            # refresh-detected drift gets its own section
  outputs: false

# Curated rule sets shipped with tgsieve. See "tgsieve presets".
extends: []
#  - builtin/aws-tags
#  - builtin/k8s-annotations
#  - builtin/computed-hashes

# Each rule needs 'attrs'. Globs: * stays inside a path segment group,
# ** matches anything. Use attrs: ["*"] to drop the whole resource.
ignore: []
#  - name: tag churn
#    attrs: ["tags.LastModified", "tags.git_commit", "tags_all.*"]
#  - name: ecs task revision
#    type: aws_ecs_task_definition
#    attrs: ["revision"]
#  - name: dev is not interesting
#    unit: "envs/dev/**"
#    attrs: ["*"]

never_hide:
  actions: [delete, replace]   # safety net: rules can never mask these
  types: []

collapse:
  instances: true         # foo[0], foo[1], foo[2] with the same diff -> one row
  cross_unit: true        # the same diff in many units -> one block
  cross_unit_mode: shape  # "shape" ignores values, "strict" requires equal values
  min_units: 2

normalize:
  empty_as_null: false    # true treats "", [], {} and null as the same value
  reorder: show           # "ignore" drops collections whose members only moved
`

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("C", "", "write the config here instead of the project root")
	here := fs.Bool("here", false, "write into the current directory")
	force := fs.Bool("force", false, "overwrite an existing file")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "tgsieve init [flags]\n\nWrites a starter .tgsieve.yaml at the project root\n"+
			"(git root, else the top terragrunt config directory).\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := *dir
	if target == "" {
		if *here {
			target = "."
		} else {
			root, err := config.ProjectRoot(".")
			if err != nil {
				return err
			}
			target = root
		}
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", target)
	}
	path := filepath.Join(target, ".tgsieve.yaml")
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}
	if err := os.WriteFile(path, []byte(starterConfig), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

func deref(b *bool) bool {
	return b != nil && *b
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func useColor(noColor bool) bool {
	if noColor || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTTY(os.Stdout)
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
