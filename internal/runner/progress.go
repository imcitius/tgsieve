package runner

import (
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/imcitius/tgsieve/internal/textutil"
)

var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// Progress draws the single status line that replaces terraform's wall of
// text while the run is in flight. Errors are printed as they happen, because
// waiting for a 20-minute run to end before learning a unit failed is the
// other half of the problem.
type Progress struct {
	w       io.Writer
	tty     bool
	verbose bool
	Color   bool
	// Verb names what the run is doing, for the status line.
	Verb string

	mu        sync.Mutex
	units     map[string]bool
	total     int
	planned   int
	refreshed int
	resources int
	// track limits what counts towards progress. An apply visits every unit
	// in the queue, but only the ones with changes are doing anything, and
	// counting the rest makes a one-unit apply look like a stack-wide one.
	track map[string]bool
	done  map[string]bool
	// seen counts each distinct kind of error, so a diagnostic repeated once
	// per resource is announced once rather than scrolling the real output off
	// the screen.
	seen       map[string]int
	suppressed int
	errors     int
	frame      int
	start      time.Time
	lastBeat   time.Time
	finished   bool
}

// heartbeat is how often a non-tty run reports that it is still alive.
const heartbeat = 30 * time.Second

// Refreshed counts one resource whose state terraform just re-read.
func (p *Progress) Refreshed() {
	p.mu.Lock()
	p.refreshed++
	p.mu.Unlock()
}

// PlannedResource counts one resource terraform decided to change.
func (p *Progress) PlannedResource() {
	p.mu.Lock()
	p.resources++
	p.mu.Unlock()
}

// SetTotal records how many units the run is expected to cover. A tracked set
// wins: the caller knew which units matter before the queue was measured, and
// the queue includes everything the run merely walks past.
func (p *Progress) SetTotal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.track != nil {
		return
	}
	p.total = n
}

// Track narrows progress to a known set of units and makes their count the
// total, for phases where most of the queue has nothing to do.
func (p *Progress) Track(units []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.track = make(map[string]bool, len(units))
	for _, u := range units {
		p.track[u] = true
	}
	p.done = map[string]bool{}
	p.total = len(units)
}

// UnitDone marks a tracked unit as finished.
func (p *Progress) UnitDone(dir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.track == nil {
		return
	}
	if dir == "" && len(p.track) == 1 {
		// A single-unit run is its own working directory, which terragrunt
		// does not bother to name.
		for u := range p.track {
			dir = u
		}
	}
	if p.track[dir] {
		p.done[dir] = true
	}
}

func NewProgress(w io.Writer, tty, verbose bool) *Progress {
	now := time.Now()
	return &Progress{w: w, tty: tty, verbose: verbose, Verb: "planning", units: map[string]bool{}, start: now, lastBeat: now}
}

func (p *Progress) Unit(dir string) {
	if dir == "" {
		return
	}
	p.mu.Lock()
	p.units[dir] = true
	p.mu.Unlock()
}

// errorLine caps how much of a diagnostic the live feed shows. It is a
// heads-up while the run is going; the report underneath carries the detail.
const errorLine = 120

func (p *Progress) Error(msg string) {
	msg = strings.TrimSpace(msg)
	p.mu.Lock()
	p.errors++
	if p.seen == nil {
		p.seen = map[string]int{}
	}
	key := textutil.NormalizeError(textutil.Headline(msg))
	p.seen[key]++
	repeat := p.seen[key] > 1
	if repeat {
		p.suppressed++
	}
	p.mu.Unlock()
	if repeat {
		p.draw()
		return
	}
	p.clear()
	fmt.Fprintf(p.w, "%s %s\n", p.red("✗"), truncate(msg, errorLine))
	p.draw()
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func (p *Progress) Note(level, msg string) {
	if !p.verbose || msg == "" {
		return
	}
	p.clear()
	fmt.Fprintf(p.w, "  %s %s\n", p.dim(level), strings.TrimSpace(msg))
	p.draw()
}

func (p *Progress) Raw(line string) {
	if !p.verbose {
		return
	}
	p.clear()
	fmt.Fprintln(p.w, p.dim(line))
	p.draw()
}

// Watch polls the plan directory so the status line can report how many units
// have actually produced a plan, and returns a channel to stop it.
func (p *Progress) Watch(planDir string) chan struct{} {
	stop := make(chan struct{})
	go func() {
		interval := 120 * time.Millisecond
		if !p.tty {
			// No spinner to animate; just poll often enough for the heartbeat
			// to be roughly on time.
			interval = 2 * time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		poll := 0
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				poll++
				if p.tracking() {
					// Progress comes from the run's own events, not from files
					// appearing on disk.
				} else if !p.tty || poll%4 == 0 {
					n := countPlans(planDir)
					p.mu.Lock()
					p.planned = n
					p.mu.Unlock()
				}
				p.mu.Lock()
				p.frame++
				p.mu.Unlock()
				if p.tty {
					p.draw()
				} else {
					p.beat()
				}
			}
		}
	}()
	return stop
}

func (p *Progress) Done() {
	p.mu.Lock()
	p.finished = true
	suppressed := p.suppressed
	p.mu.Unlock()
	p.clear()
	if suppressed > 0 {
		noun := "errors"
		if suppressed == 1 {
			noun = "error"
		}
		fmt.Fprintf(p.w, "%s\n", p.dim(fmt.Sprintf(
			"  %d more %s of kinds already shown — see the report below", suppressed, noun)))
	}
}

// pastVerb describes finished work: "applying" counts things applied. Caller
// holds the lock.
func (p *Progress) pastVerb() string {
	if p.verb() == "applying" {
		return "applied"
	}
	return "planned"
}

// verb is what to call what is happening. Caller holds the lock.
func (p *Progress) verb() string {
	if p.Verb == "" {
		return "planning"
	}
	return p.Verb
}

// beat prints a plain liveness line for logs that cannot show a spinner.
func (p *Progress) beat() {
	p.mu.Lock()
	if p.finished || time.Since(p.lastBeat) < heartbeat {
		p.mu.Unlock()
		return
	}
	p.lastBeat = time.Now()
	line := fmt.Sprintf("… %s · %s · %d units seen · %s elapsed",
		p.verb(), p.progressLabel(), len(p.units), time.Since(p.start).Truncate(time.Second))
	if p.errors > 0 {
		line += fmt.Sprintf(" · %d failed", p.errors)
	}
	p.mu.Unlock()
	fmt.Fprintln(p.w, line)
}

// progressLabel describes how far the run has got. Caller holds the lock.
//
// A stack is measured in units ("7/28 planned"); a single unit has no such
// scale, so it is measured in the resources terraform reports as it goes.
func (p *Progress) progressLabel() string {
	if p.track != nil {
		return fmt.Sprintf("%d/%d %s", len(p.done), p.total, p.pastVerb())
	}
	if p.total == 1 {
		switch {
		case p.resources > 0:
			return fmt.Sprintf("%s refreshed · %d to change",
				plural(p.refreshed, "resource"), p.resources)
		case p.refreshed > 0:
			return plural(p.refreshed, "resource") + " refreshed"
		}
		return "1 unit"
	}
	label := fmt.Sprintf("%d planned", p.planned)
	if p.total > 0 {
		label = fmt.Sprintf("%d/%d planned", p.planned, p.total)
	}
	// Units terragrunt has started but not finished. The rest of the queue is
	// waiting on the DAG, which is worth distinguishing from a stalled run.
	if running := len(p.units) - p.planned; running > 0 {
		label += fmt.Sprintf(" · %d running", running)
	}
	return label
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (p *Progress) draw() {
	if !p.tty {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	s := string(spinner[p.frame%len(spinner)])
	line := fmt.Sprintf("%s %s · %s · %s",
		s, p.verb(), p.progressLabel(), time.Since(p.start).Truncate(time.Second))
	if p.errors > 0 {
		line += fmt.Sprintf(" · %s", p.red(fmt.Sprintf("%d failed", p.errors)))
	}
	fmt.Fprintf(p.w, "\r\033[2K%s", line)
}

func (p *Progress) tracking() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.track != nil
}

func (p *Progress) clear() {
	if p.tty {
		fmt.Fprint(p.w, "\r\033[2K")
	}
}

func countPlans(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == planFileName {
			n++
		}
		return nil
	})
	return n
}

func (p *Progress) red(s string) string {
	if !p.Color {
		return s
	}
	return "\033[31m" + s + "\033[0m"
}

func (p *Progress) dim(s string) string {
	if !p.Color {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}
