package version

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewerComparesVersionsNotStrings(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.9.1", "0.10.0", true}, // string comparison would say no
		{"0.10.0", "0.9.1", false},
		{"1.0.0", "1.0.0", false},
		{"v0.9.0", "v0.9.1", true},
		{"0.9.0", "0.9.0-rc1", false}, // a pre-release of the same version
		{"0.9.0", "1.0.0", true},
	}
	for _, c := range cases {
		if got := newer(c.current, c.latest); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestLocalBuildsAreNeverOutOfDate(t *testing.T) {
	if looksReleased("dev") || looksReleased("") {
		t.Error("a local build has no version to compare")
	}
	if !looksReleased("0.10.0") {
		t.Error("a released version should be checked")
	}
}

func TestStartRespectsTheOptOut(t *testing.T) {
	t.Setenv(DisableEnv, "1")
	if c := Start(context.Background(), "0.1.0"); c != nil {
		t.Error("the check must be switchable off")
	}
}

func TestFreshCacheIsUsedWithoutAskingAgain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv(DisableEnv, "")
	t.Setenv("NO_UPDATE_NOTIFIER", "")

	path := filepath.Join(dir, "tgsieve", "version-check.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeState(path, state{Latest: "9.9.9", CheckedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	c := Start(context.Background(), "0.1.0")
	if c == nil {
		t.Fatal("expected a checker")
	}
	if c.result != nil {
		t.Error("a fresh answer should not trigger another lookup")
	}
	if notice := c.Notice(0); notice == "" {
		t.Error("a newer version in the cache should be reported")
	}
}

func TestGraceIsProportionateToTheRun(t *testing.T) {
	// A command that returned instantly should not be held up by a courtesy
	// check; one that already took a minute can spare a moment.
	if Grace(200*time.Millisecond) != 0 {
		t.Error("an instant command should not wait at all")
	}
	if g := Grace(3 * time.Second); g <= 0 || g > time.Second {
		t.Errorf("a short run should wait briefly, got %v", g)
	}
	if Grace(2*time.Minute) != LookupTimeout {
		t.Error("a long run can wait for the whole lookup")
	}
}

func TestNoticeIsSilentWhenCurrent(t *testing.T) {
	c := &Checker{current: "1.2.3", known: "1.2.3"}
	if got := c.Notice(0); got != "" {
		t.Errorf("nothing to say, said %q", got)
	}
	var nilChecker *Checker
	if got := nilChecker.Notice(0); got != "" {
		t.Errorf("a disabled checker says nothing, said %q", got)
	}
}
