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
  tgsieve init                                      write a starter .tgsieve.yaml at the project root
  tgsieve version

The stack is never planned implicitly: without --all only the unit in the
working directory runs.

Examples:
  tgsieve plan                                  # this unit only
  tgsieve plan --all                            # the whole stack below here
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
		os.Exit(1)
	}
	os.Exit(code)
}

type commonFlags struct {
	dir        string
	configPath string
	verbose    bool
	showEmpty  bool
	explain    bool
	noSieve    bool
	noColor    bool
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
	fs.IntVar(&c.maxAttrs, "max-attrs", 12, "max attributes shown per resource")
	fs.IntVar(&c.maxUnits, "max-units", 6, "max unit names listed per collapsed group")
}

func (c *commonFlags) renderOpts() render.Options {
	return render.Options{
		Color:     useColor(c.noColor),
		Verbose:   c.verbose,
		ShowEmpty: c.showEmpty,
		Explain:   c.explain,
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
	tgArgs := fs.String("tg-args", "", "extra terragrunt flags, space separated (e.g. \"--filter-affected\")")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "tgsieve plan [flags] [-- <tofu/terraform args>]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1, err
	}

	cfg, err := cf.loadConfig()
	if err != nil {
		return 1, err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	color := useColor(cf.noColor)
	prog := runner.NewProgress(os.Stderr, isTTY(os.Stderr), cf.verbose)
	prog.Color = color

	res, err := runner.Run(ctx, runner.Options{
		Dir:            cf.dir,
		Binary:         *binary,
		All:            *all,
		Command:        "plan",
		TFArgs:         fs.Args(),
		TerragruntArgs: strings.Fields(*tgArgs),
		JSONOutDir:     *keepPlans,
		OutDir:         *outDir,
		TFPath:         *tfPath,
		Progress:       prog,
	})
	if err != nil {
		return 1, err
	}

	rep := sieve.Apply(res.Run, cfg)
	render.TTY(os.Stdout, rep, cf.renderOpts())

	if len(rep.ErroredUnits) > 0 || (res.ExitCode != 0 && len(res.Run.Units) == 0) {
		return 1, nil
	}
	if *detailed && rep.HasChanges() {
		return 2, nil
	}
	return 0, nil
}

func cmdShow(args []string) (int, error) {
	if len(args) == 0 {
		return 1, fmt.Errorf("show needs a plan directory (the one you passed to --keep-plans)")
	}
	dir := args[0]
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	var cf commonFlags
	cf.bind(fs)
	detailed := fs.Bool("detailed-exitcode", false, "exit 2 when changes survive the sieve")
	if err := fs.Parse(args[1:]); err != nil {
		return 1, err
	}
	cfg, err := cf.loadConfig()
	if err != nil {
		return 1, err
	}
	run, err := runner.Collect(dir)
	if err != nil {
		return 1, err
	}
	if len(run.Units) == 0 {
		return 1, fmt.Errorf("no tfplan.json found under %s", dir)
	}
	rep := sieve.Apply(run, cfg)
	render.TTY(os.Stdout, rep, cf.renderOpts())
	if *detailed && rep.HasChanges() {
		return 2, nil
	}
	return 0, nil
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
	fmt.Printf("never_hide: actions=%v types=%v\n", cfg.NeverHide.Actions, cfg.NeverHide.Types)
	fmt.Printf("\nignore rules (%d):\n", len(cfg.Ignore))
	for i, r := range cfg.Ignore {
		fmt.Printf("  %s\n", r.Label(i))
	}
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
