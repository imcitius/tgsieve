package runner

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Activity tracks what terraform is doing right now, so a long apply shows its
// work instead of a spinner over silence. The lines terraform prints are
// already reaching us — terragrunt wraps each one with the unit it came from —
// they were simply being discarded as noise.
type Activity struct {
	mu      sync.Mutex
	entries map[string]*activityEntry
	seq     int
}

type activityEntry struct {
	unit    string
	addr    string
	verb    string
	started time.Time
	ended   time.Time
	seq     int
}

func NewActivity() *Activity {
	return &Activity{entries: map[string]*activityEntry{}}
}

// terraform announces each step in a stable shape:
//
//	aws_instance.web: Creating...
//	aws_instance.web: Still creating... [10s elapsed]
//	aws_instance.web: Creation complete after 12s [id=i-0abc]
var activityRe = regexp.MustCompile(`^(\S.*?): (Creating|Still creating|Creation complete|Destroying|Still destroying|Destruction complete|Modifying|Still modifying|Modifications complete|Refreshing state|Reading|Still reading|Read complete|Provisioning with)`)

// Observe records a line of terraform output, reporting whether it was one it
// recognised.
func (a *Activity) Observe(unit, line string) bool {
	m := activityRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return false
	}
	addr, phrase := m[1], m[2]
	verb, done := phaseOf(phrase)
	a.mark(unit, addr, verb, done)
	return true
}

// Event records a structured terraform event, which the single-unit and
// terraform-engine paths get instead of text.
func (a *Activity) Event(unit, kind, addr, action string) {
	if addr == "" {
		return
	}
	switch kind {
	case "apply_start":
		a.mark(unit, addr, applyVerb(action), false)
	case "apply_progress":
		a.mark(unit, addr, applyVerb(action), false)
	case "apply_complete":
		a.mark(unit, addr, applyVerb(action), true)
	case "refresh_start":
		a.mark(unit, addr, "refreshing", false)
	case "refresh_complete":
		a.mark(unit, addr, "refreshing", true)
	}
}

func applyVerb(action string) string {
	switch action {
	case "create":
		return "creating"
	case "update":
		return "modifying"
	case "delete":
		return "destroying"
	case "read":
		return "reading"
	}
	return "working"
}

func phaseOf(phrase string) (verb string, done bool) {
	switch phrase {
	case "Creating", "Still creating":
		return "creating", false
	case "Creation complete":
		return "creating", true
	case "Destroying", "Still destroying":
		return "destroying", false
	case "Destruction complete":
		return "destroying", true
	case "Modifying", "Still modifying":
		return "modifying", false
	case "Modifications complete":
		return "modifying", true
	case "Refreshing state":
		return "refreshing", false
	case "Reading", "Still reading":
		return "reading", false
	case "Read complete":
		return "reading", true
	case "Provisioning with":
		return "provisioning", false
	}
	return "working", false
}

func (a *Activity) mark(unit, addr, verb string, done bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := unit + "\x00" + addr
	e, ok := a.entries[key]
	if !ok {
		a.seq++
		e = &activityEntry{unit: unit, addr: addr, started: time.Now(), seq: a.seq}
		a.entries[key] = e
	}
	e.verb = verb
	if done && e.ended.IsZero() {
		e.ended = time.Now()
	}
}

// Lines renders the window: what is still running, longest first, then the
// most recently finished to fill the space. Watching the same slow resource
// stay at the top is the point.
func (a *Activity) Lines(max int, width int) []string {
	if max <= 0 {
		return nil
	}
	a.mu.Lock()
	running := make([]*activityEntry, 0, len(a.entries))
	finished := make([]*activityEntry, 0, len(a.entries))
	for _, e := range a.entries {
		if e.ended.IsZero() {
			running = append(running, e)
		} else {
			finished = append(finished, e)
		}
	}
	a.mu.Unlock()

	sort.Slice(running, func(i, j int) bool { return running[i].started.Before(running[j].started) })
	sort.Slice(finished, func(i, j int) bool { return finished[i].ended.After(finished[j].ended) })

	unitWidth, addrWidth := 0, 0
	pick := append([]*activityEntry{}, running...)
	for _, e := range finished {
		if len(pick) >= max {
			break
		}
		pick = append(pick, e)
	}
	if len(pick) > max {
		pick = pick[:max]
	}
	for _, e := range pick {
		if len(e.unit) > unitWidth {
			unitWidth = len(e.unit)
		}
		if len(e.addr) > addrWidth {
			addrWidth = len(e.addr)
		}
	}
	if unitWidth > 28 {
		unitWidth = 28
	}
	if addrWidth > 44 {
		addrWidth = 44
	}

	now := time.Now()
	out := make([]string, 0, len(pick))
	for _, e := range pick {
		status := e.verb + "…"
		took := now.Sub(e.started)
		if !e.ended.IsZero() {
			status = "done"
			took = e.ended.Sub(e.started)
		}
		line := fmt.Sprintf("  %-*s  %-*s  %s %s",
			unitWidth, clip(e.unit, unitWidth),
			addrWidth, clip(e.addr, addrWidth),
			status, took.Truncate(time.Second))
		out = append(out, clip(line, width))
	}
	return out
}

// Running counts what is in flight, for the status line.
func (a *Activity) Running() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.entries {
		if e.ended.IsZero() {
			n++
		}
	}
	return n
}

func clip(s string, width int) string {
	if width <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}
