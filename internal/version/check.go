// Package version checks, quietly and in the background, whether a newer
// release exists.
//
// Two rules shape it. It must never delay or fail a run: a plan waiting on
// github, or breaking because github is down, would be a poor trade for a
// notice. And it must be easy to turn off, because a tool that reaches out to
// the network without being asked should always answer to something.
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Interval is how often the latest release is looked up. The answer changes
// rarely and nobody needs it sooner.
const Interval = 24 * time.Hour

// LookupTimeout bounds the request, which runs alongside the real work.
const LookupTimeout = 3 * time.Second

const releasesURL = "https://api.github.com/repos/imcitius/tgsieve/releases/latest"

// DisableEnv turns the check off entirely.
const DisableEnv = "TGSIEVE_NO_VERSION_CHECK"

type state struct {
	Latest    string    `json:"latest"`
	CheckedAt time.Time `json:"checked_at"`
}

// Checker looks up the latest release without holding anything up.
type Checker struct {
	current string
	cache   string
	result  chan string
	known   string
}

// Start reads what is already known and, if that is stale, begins a lookup in
// the background. It returns nil when checking is switched off or pointless.
func Start(ctx context.Context, current string) *Checker {
	if Disabled() || !looksReleased(current) {
		return nil
	}
	c := &Checker{current: current, cache: cachePath()}
	if s, err := readState(c.cache); err == nil {
		c.known = s.Latest
		if time.Since(s.CheckedAt) < Interval {
			return c
		}
	}
	c.result = make(chan string, 1)
	go func() {
		latest := fetchLatest(ctx)
		if latest != "" {
			_ = writeState(c.cache, state{Latest: latest, CheckedAt: time.Now()})
		}
		c.result <- latest
	}()
	return c
}

// Notice returns the message to show, or empty when there is nothing to say.
//
// A lookup still in flight gets a short grace period, and only when the run
// itself took long enough that waiting is proportionate — a command that
// finished instantly should not be held up by a courtesy check. Without any
// wait the process would exit before the answer arrived, and the cache would
// never fill, so the notice would never appear at all.
func (c *Checker) Notice(grace time.Duration) string {
	if c == nil {
		return ""
	}
	latest := c.known
	if c.result != nil {
		if grace <= 0 {
			select {
			case fetched := <-c.result:
				if fetched != "" {
					latest = fetched
				}
			default:
			}
		} else {
			timer := time.NewTimer(grace)
			defer timer.Stop()
			select {
			case fetched := <-c.result:
				if fetched != "" {
					latest = fetched
				}
			case <-timer.C:
			}
		}
	}
	if latest == "" || !newer(c.current, latest) {
		return ""
	}
	return fmt.Sprintf("tgsieve %s is available (you have %s) — brew upgrade --cask imcitius/tap/tgsieve",
		latest, c.current)
}

// Grace decides how long the notice may wait for an answer, given how long the
// command itself took. A run measured in seconds can spare a moment; one that
// returned instantly cannot.
func Grace(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < time.Second:
		return 0
	case elapsed < 5*time.Second:
		return 500 * time.Millisecond
	default:
		return LookupTimeout
	}
}

// Disabled reports whether the user asked not to be checked up on.
func Disabled() bool {
	return os.Getenv(DisableEnv) != "" || os.Getenv("NO_UPDATE_NOTIFIER") != ""
}

// looksReleased skips builds that have no version to compare: a local build
// is not out of date.
func looksReleased(v string) bool {
	v = strings.TrimPrefix(v, "v")
	if v == "" || v == "dev" {
		return false
	}
	return v[0] >= '0' && v[0] <= '9'
}

func fetchLatest(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, LookupTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "tgsieve")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(body.TagName), "v")
}

// newer reports whether latest is a higher version than current.
func newer(current, latest string) bool {
	c, l := parse(current), parse(latest)
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// parse reads major.minor.patch, ignoring any pre-release suffix.
func parse(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return out
		}
		out[i] = n
	}
	return out
}

func cachePath() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "tgsieve", "version-check.json")
}

func readState(path string) (state, error) {
	var s state
	if path == "" {
		return s, os.ErrNotExist
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func writeState(path string, s state) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
