package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/sieve"
)

func report(counts model.Counts) *sieve.Report {
	return &sieve.Report{Kept: counts, UnitsChanged: 3}
}

func TestApproveRefusesWithoutATerminal(t *testing.T) {
	var out bytes.Buffer
	ok, err := approve(strings.NewReader(""), &out, report(model.Counts{Update: 2}), false, false)
	if ok {
		t.Fatal("an apply must never proceed unasked")
	}
	if err == nil || !strings.Contains(err.Error(), "--auto-approve") {
		t.Errorf("the error should name the way out: %v", err)
	}
}

func TestApproveNeedsExactlyYes(t *testing.T) {
	for _, answer := range []string{"no\n", "y\n", "YES please\n", "\n", ""} {
		var out bytes.Buffer
		ok, _ := approve(strings.NewReader(answer), &out, report(model.Counts{Update: 2}), false, true)
		if ok {
			t.Errorf("%q should not have been taken as consent", answer)
		}
	}
	var out bytes.Buffer
	ok, err := approve(strings.NewReader("yes\n"), &out, report(model.Counts{Update: 2}), false, true)
	if err != nil || !ok {
		t.Errorf("a plain yes on a non-destructive plan should proceed: %v %v", ok, err)
	}
}

func TestApproveAsksTwiceWhenDestructive(t *testing.T) {
	counts := model.Counts{Update: 1, Delete: 2, Replace: 1}

	// Saying yes once is not enough.
	var out bytes.Buffer
	if ok, _ := approve(strings.NewReader("yes\n"), &out, report(counts), false, true); ok {
		t.Error("a destroy needs its own confirmation")
	}
	if !strings.Contains(out.String(), "destroyed or replaced") {
		t.Errorf("the second prompt should say what is at stake: %q", out.String())
	}

	// The wrong second word is a refusal.
	out.Reset()
	if ok, _ := approve(strings.NewReader("yes\nyes\n"), &out, report(counts), false, true); ok {
		t.Error("the second prompt asks for a specific word")
	}

	out.Reset()
	ok, err := approve(strings.NewReader("yes\ndestroy\n"), &out, report(counts), false, true)
	if err != nil || !ok {
		t.Errorf("yes then destroy should proceed: %v %v", ok, err)
	}
}

func TestAutoApproveSaysWhatItIsDestroying(t *testing.T) {
	var out bytes.Buffer
	ok, err := approve(strings.NewReader(""), &out, report(model.Counts{Delete: 3}), true, false)
	if err != nil || !ok {
		t.Fatalf("--auto-approve proceeds: %v %v", ok, err)
	}
	if !strings.Contains(out.String(), "3 resources") {
		t.Errorf("it should still state the damage: %q", out.String())
	}
}
