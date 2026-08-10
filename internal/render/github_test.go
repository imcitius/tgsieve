package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imcitius/tgsieve/internal/model"
	"github.com/imcitius/tgsieve/internal/sieve"
)

func TestGitHubAnnotatesTheFailingLines(t *testing.T) {
	rep := &sieve.Report{
		UnitsTotal:   1,
		ErroredUnits: []model.Unit{{Path: "infra/networking", Errored: true}},
		Failures: []sieve.FailureGroup{{
			Headline:  "Error: Unsupported attribute",
			Units:     []string{"infra/networking"},
			Count:     2,
			Locations: []string{"modules-vpcs.tf:72", "modules-vpcs.tf:73"},
			Detail:    []string{"Error: Unsupported attribute", "72: local_cidr_block = local.vpcs.x"},
		}},
	}
	var buf bytes.Buffer
	GitHub(&buf, rep, Options{})
	got := buf.String()

	for _, want := range []string{
		"::error file=modules-vpcs.tf,line=72,",
		"::error file=modules-vpcs.tf,line=73,",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing annotation %q in:\n%s", want, got)
		}
	}
	// Newlines inside a message would end the command early.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.HasPrefix(line, "::") {
			t.Errorf("a message broke out of its workflow command: %q", line)
		}
	}
}

func TestGitHubWarnsAboutDestruction(t *testing.T) {
	rep := &sieve.Report{Kept: model.Counts{Delete: 2, Replace: 1}, UnitsChanged: 3}
	var buf bytes.Buffer
	GitHub(&buf, rep, Options{})
	got := buf.String()

	if !strings.Contains(got, "::warning title=tgsieve::") {
		t.Errorf("destruction deserves a warning annotation:\n%s", got)
	}
	if !strings.Contains(got, "::notice title=tgsieve::") {
		t.Errorf("the summary should always be reported:\n%s", got)
	}
}

func TestGitHubEscapesPropertiesThatWouldSplitTheCommand(t *testing.T) {
	if got := escapeProperty("a,b:c"); got != "a%2Cb%3Ac" {
		t.Errorf("escapeProperty = %q", got)
	}
	if got := escapeWorkflow("line one\nline two"); strings.Contains(got, "\n") {
		t.Errorf("escapeWorkflow left a newline: %q", got)
	}
}

func TestGitHubMessageDoesNotNameADifferentLine(t *testing.T) {
	// The annotation is placed on a line; a message naming another one reads
	// as a contradiction.
	rep := &sieve.Report{
		UnitsTotal:   1,
		ErroredUnits: []model.Unit{{Path: "u", Errored: true}},
		Failures: []sieve.FailureGroup{{
			Headline:  `Error: Unsupported attribute at main.tf:20: This object does not have an attribute named "x".`,
			Units:     []string{"u"},
			Count:     2,
			Locations: []string{"main.tf:16", "main.tf:20"},
			Detail:    []string{"Error: Unsupported attribute at main.tf:20: …", "20: tables = local.vpcs.x"},
		}},
	}
	var buf bytes.Buffer
	GitHub(&buf, rep, Options{})

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !strings.HasPrefix(line, "::error file=main.tf,line=16") {
			continue
		}
		if strings.Contains(line, "main.tf:20") || strings.Contains(line, "20:") {
			t.Errorf("annotation on line 16 talks about line 20: %q", line)
		}
		if !strings.Contains(line, "Unsupported attribute") {
			t.Errorf("annotation lost the message: %q", line)
		}
	}
}
