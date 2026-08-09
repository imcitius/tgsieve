package runner

import (
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

	mu       sync.Mutex
	units    map[string]bool
	total    int
	planned  int
	errors   int
	frame    int
	start    time.Time
	lastBeat time.Time
	finished bool
}

// heartbeat is how often a non-tty run reports that it is still alive.
const heartbeat = 30 * time.Second

// SetTotal records how many units the run is expected to cover.
func (p *Progress) SetTotal(n int) {
	p.mu.Lock()
	p.total = n
	p.mu.Unlock()
}

func NewProgress(w io.Writer, tty, verbose bool) *Progress {
	now := time.Now()
	return &Progress{w: w, tty: tty, verbose: verbose, units: map[string]bool{}, start: now, lastBeat: now}
}

func (p *Progress) Unit(dir string) {
	if dir == "" {
		return
	}
	p.mu.Lock()
	p.units[dir] = true
	p.mu.Unlock()
}

func (p *Progress) Error(msg string) {
	p.mu.Lock()
	p.errors++
	p.mu.Unlock()
	p.clear()
	fmt.Fprintf(p.w, "%s %s\n", p.red("✗"), strings.TrimSpace(msg))
	p.draw()
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
				if !p.tty || poll%4 == 0 {
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
	p.mu.Unlock()
	p.clear()
}

// beat prints a plain liveness line for logs that cannot show a spinner.
func (p *Progress) beat() {
	p.mu.Lock()
	if p.finished || time.Since(p.lastBeat) < heartbeat {
		p.mu.Unlock()
		return
	}
	p.lastBeat = time.Now()
	line := fmt.Sprintf("… %s · %d units seen · %s elapsed",
		p.progressLabel(), len(p.units), time.Since(p.start).Truncate(time.Second))
	if p.errors > 0 {
		line += fmt.Sprintf(" · %d failed", p.errors)
	}
	p.mu.Unlock()
	fmt.Fprintln(p.w, line)
}

// progressLabel is "7/28 planned" when the queue size is known, "7 planned"
// when it is not. Caller holds the lock.
func (p *Progress) progressLabel() string {
	if p.total > 0 {
		return fmt.Sprintf("%d/%d planned", p.planned, p.total)
	}
	return fmt.Sprintf("%d planned", p.planned)
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
	line := fmt.Sprintf("%s planning · %s · %s",
		s, p.progressLabel(), time.Since(p.start).Truncate(time.Second))
	if p.errors > 0 {
		line += fmt.Sprintf(" · %s", p.red(fmt.Sprintf("%d failed", p.errors)))
	}
	fmt.Fprintf(p.w, "\r\033[2K%s", line)
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
