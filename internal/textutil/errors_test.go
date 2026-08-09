package textutil

import "testing"

const raw = "Run failed: error occurred:\n\n* Failed to execute \"terraform plan\" in ./envs/dev/a\n  \x1b[31m╷\x1b[0m\x1b[0m\n  \x1b[31m│\x1b[0m \x1b[0m\x1b[1m\x1b[31mError: \x1b[0m\x1b[0m\x1b[1mInvalid value for input variable\x1b[0m\n  \x1b[31m│\x1b[0m \x1b[0m\n  \x1b[31m│\x1b[0m \x1b[0m  on main.tf line 12:\n  \x1b[31m╵\x1b[0m\x1b[0m\n"

func TestCleanErrorDropsFramingAndColor(t *testing.T) {
	got := CleanError(raw, 0)
	if len(got) == 0 {
		t.Fatal("everything was stripped")
	}
	if got[0] != "Error: Invalid value for input variable" {
		t.Errorf("first line = %q, want the Error: line", got[0])
	}
	for _, l := range got {
		for _, bad := range []string{"\x1b", "│", "╷", "╵"} {
			if contains(l, bad) {
				t.Errorf("line %q still contains %q", l, bad)
			}
		}
	}
}

func TestCleanErrorCaps(t *testing.T) {
	got := CleanError(raw, 1)
	if len(got) != 2 || got[1] != "…" {
		t.Errorf("cap not applied: %v", got)
	}
	if len(CleanError(raw, 50)) != 2 {
		t.Error("a cap above the line count must not add an ellipsis")
	}
}

func TestHeadline(t *testing.T) {
	if got := Headline(raw); got != "Error: Invalid value for input variable" {
		t.Errorf("Headline = %q", got)
	}
	if got := Headline("plain failure"); got != "plain failure" {
		t.Errorf("Headline fallback = %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
