package render

import "testing"

func TestOthersHidesTheLocationTheDiagnosticAlreadyShows(t *testing.T) {
	detail := []string{
		"Error: Missing required argument",
		`on main.tf line 5, in module "env":`,
	}
	// Repeating "also at main.tf:5" under a message that already says line 5
	// is a line the reader has to read to learn nothing.
	if got := others([]string{"main.tf:5"}, detail); len(got) != 0 {
		t.Errorf("already shown, should be dropped: %v", got)
	}
	if got := others([]string{"main.tf:5", "vpc.tf:12"}, detail); len(got) != 1 || got[0] != "vpc.tf:12" {
		t.Errorf("a second place is news: %v", got)
	}
}
