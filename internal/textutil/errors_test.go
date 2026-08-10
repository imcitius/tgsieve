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

const stackBlob = `Run failed: 2 errors occurred:

* Failed to execute "tofu plan" in ./envs/dev/a/.terragrunt-cache/abc123/def456
  Error: Reference to undeclared input variable
  on main.tf line 56:
  exit status 1

* Failed to execute "tofu plan" in ./envs/prod/b/.terragrunt-cache/xyz789/uvw000
  Error: Something else entirely
  exit status 1
`

func TestSplitErrorsSeparatesUnits(t *testing.T) {
	parts := SplitErrors(stackBlob)
	if len(parts) != 3 {
		t.Fatalf("want the preamble plus one part per failure, got %d: %#v", len(parts), parts)
	}
	if !contains(parts[1], "envs/dev/a") || contains(parts[1], "envs/prod/b") {
		t.Errorf("part 1 should be only dev/a's failure: %q", parts[1])
	}
	if !contains(parts[2], "Something else entirely") {
		t.Errorf("part 2 lost its diagnostic: %q", parts[2])
	}
}

func TestSplitErrorsPassesThroughSingleErrors(t *testing.T) {
	if got := SplitErrors("Error: just one thing"); len(got) != 1 || got[0] != "Error: just one thing" {
		t.Errorf("SplitErrors = %#v", got)
	}
}

func TestCleanErrorDropsCachePaths(t *testing.T) {
	in := `Failed to execute "tofu plan" in ./envs/dev/a/.terragrunt-cache/abc123/def456`
	got := CleanError(in, 0)
	if len(got) != 1 || got[0] != `Failed to execute "tofu plan" in ./envs/dev/a` {
		t.Errorf("cache path not trimmed: %#v", got)
	}
}

func TestNormalizeErrorFoldsIncidentalDifferences(t *testing.T) {
	same := []string{
		`Error: creating S3 Bucket (logs-prod-eu): AccessDenied`,
		`Error: creating S3 Bucket (logs-stage-us): AccessDenied`,
	}
	if NormalizeError(same[0]) != NormalizeError(same[1]) {
		t.Errorf("bucket names should not split a group:\n  %q\n  %q",
			NormalizeError(same[0]), NormalizeError(same[1]))
	}

	byResource := []string{
		`Error: Invalid count argument on aws_instance.web[0]`,
		`Error: Invalid count argument on aws_instance.db[3]`,
	}
	if NormalizeError(byResource[0]) != NormalizeError(byResource[1]) {
		t.Errorf("resource addresses should not split a group:\n  %q\n  %q",
			NormalizeError(byResource[0]), NormalizeError(byResource[1]))
	}

	different := []string{
		`Error: no valid credential sources found`,
		`Error: Unsupported argument`,
	}
	if NormalizeError(different[0]) == NormalizeError(different[1]) {
		t.Error("genuinely different errors must stay apart")
	}
}

func TestLocationFindsBothWordings(t *testing.T) {
	cases := map[string]string{
		`Error: Unsupported attribute at modules-vpcs.tf:72: This object does not…`: "modules-vpcs.tf:72",
		"Error: Invalid value\n  on main.tf line 12, in module \"x\":":              "main.tf:12",
		"Error: something with no location at all":                                  "",
	}
	for in, want := range cases {
		if got := Location(in); got != want {
			t.Errorf("Location(%q) = %q, want %q", in, got, want)
		}
	}
}
