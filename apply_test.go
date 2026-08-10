package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/render"
	"github.com/imcitius/tgsieve/internal/runner"
	"github.com/imcitius/tgsieve/internal/sieve"
)

func report(counts model.Counts) *sieve.Report {
	return &sieve.Report{Kept: counts, UnitsChanged: 3}
}

func TestApproveRefusesWithoutATerminal(t *testing.T) {
	var out bytes.Buffer
	ok, err := approve(context.Background(), strings.NewReader(""), &out, report(model.Counts{Update: 2}), false, false)
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
		ok, _ := approve(context.Background(), strings.NewReader(answer), &out, report(model.Counts{Update: 2}), false, true)
		if ok {
			t.Errorf("%q should not have been taken as consent", answer)
		}
	}
	var out bytes.Buffer
	ok, err := approve(context.Background(), strings.NewReader("yes\n"), &out, report(model.Counts{Update: 2}), false, true)
	if err != nil || !ok {
		t.Errorf("a plain yes on a non-destructive plan should proceed: %v %v", ok, err)
	}
}

func TestApproveAsksTwiceWhenDestructive(t *testing.T) {
	counts := model.Counts{Update: 1, Delete: 2, Replace: 1}

	// Saying yes once is not enough.
	var out bytes.Buffer
	if ok, _ := approve(context.Background(), strings.NewReader("yes\n"), &out, report(counts), false, true); ok {
		t.Error("a destroy needs its own confirmation")
	}
	if !strings.Contains(out.String(), "destroyed or replaced") {
		t.Errorf("the second prompt should say what is at stake: %q", out.String())
	}

	// The wrong second word is a refusal.
	out.Reset()
	if ok, _ := approve(context.Background(), strings.NewReader("yes\nyes\n"), &out, report(counts), false, true); ok {
		t.Error("the second prompt asks for a specific word")
	}

	out.Reset()
	ok, err := approve(context.Background(), strings.NewReader("yes\ndestroy\n"), &out, report(counts), false, true)
	if err != nil || !ok {
		t.Errorf("yes then destroy should proceed: %v %v", ok, err)
	}
}

func TestAutoApproveSaysWhatItIsDestroying(t *testing.T) {
	var out bytes.Buffer
	ok, err := approve(context.Background(), strings.NewReader(""), &out, report(model.Counts{Delete: 3}), true, false)
	if err != nil || !ok {
		t.Fatalf("--auto-approve proceeds: %v %v", ok, err)
	}
	if !strings.Contains(out.String(), "3 resources") {
		t.Errorf("it should still state the damage: %q", out.String())
	}
}

func TestOutcomeNeverClaimsAnAppliedRunThatFailed(t *testing.T) {
	planned := report(model.Counts{Update: 5})
	planned.UnitsChanged = 5

	cases := []struct {
		name string
		res  runner.Result
		out  sieve.Report
	}{
		{"terragrunt exited non-zero", runner.Result{ExitCode: 1, Errors: []string{"EOF"}}, sieve.Report{}},
		{"a unit failed", runner.Result{}, sieve.Report{ErroredUnits: []model.Unit{{Path: "a", Errored: true}}}},
		{"interrupted", runner.Result{Interrupted: true}, sieve.Report{}},
	}
	for _, c := range cases {
		res := c.res
		out := c.out
		if !applyFailed(&out, &res) {
			t.Errorf("%s: should count as a failed apply", c.name)
		}
		var buf bytes.Buffer
		renderOutcome(&buf, planned, &out, &res, render.Options{})
		if strings.Contains(buf.String(), "APPLIED") && !strings.Contains(buf.String(), "APPLY") {
			t.Errorf("%s: reported success:\n%s", c.name, buf.String())
		}
		if !strings.Contains(buf.String(), "what was planned, not what landed") {
			t.Errorf("%s: does not warn that the report is not the outcome:\n%s", c.name, buf.String())
		}
	}
}

func TestOutcomeReportsSuccessPlainly(t *testing.T) {
	planned := report(model.Counts{Update: 4, Delete: 1})
	planned.UnitsChanged = 3
	res := runner.Result{}
	out := sieve.Report{}

	var buf bytes.Buffer
	renderOutcome(&buf, planned, &out, &res, render.Options{})
	got := buf.String()
	if !strings.Contains(got, "APPLIED") {
		t.Fatalf("a clean apply should say so:\n%s", got)
	}
	if !strings.Contains(got, "1 destroyed") {
		t.Errorf("destroys deserve their own line:\n%s", got)
	}
}

func TestErrorSurfacesWhenNoUnitWasBlamed(t *testing.T) {
	// terragrunt can fail before any unit runs — its own prompt reading EOF,
	// for instance. The reason still has to reach the reader.
	res := runner.Result{ExitCode: 1, Errors: []string{"EOF"}}
	out := sieve.Report{}
	var buf bytes.Buffer
	renderOutcome(&buf, report(model.Counts{Update: 1}), &out, &res, render.Options{})
	if !strings.Contains(buf.String(), "EOF") {
		t.Errorf("the error text is missing:\n%s", buf.String())
	}
}

func TestApproveGivesUpWhenInterrupted(t *testing.T) {
	// Ctrl-C during the prompt must stop the program, not be swallowed by a
	// blocking read while the terminal echoes "^C".
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() {
		ok, _ := approve(ctx, neverReads{}, io.Discard, report(model.Counts{Update: 1}), false, true)
		done <- ok
	}()

	select {
	case ok := <-done:
		if ok {
			t.Error("an interrupted prompt is not consent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approve ignored the cancelled context and kept waiting")
	}
}

// neverReads stands in for a terminal with nobody typing at it.
type neverReads struct{}

func (neverReads) Read([]byte) (int, error) { select {} }
