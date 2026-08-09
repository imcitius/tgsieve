# TODO

v1 shipped: terragrunt runner (json-out-dir + json log stream + run report),
plan parsing with attribute-level diffs, noise rules, collapsing, tty renderer,
`plan` / `show` / `rules` / `init`.

## Next up

### `tgsieve apply` — apply exactly what was reviewed
`plan --out-dir` already saves the binary plan per unit. Apply should feed those
files back (`terragrunt run --all -- apply <unit>/tfplan.tfplan`), refuse to run
if the working tree changed since the plan, and re-render the same sieved view
as a confirmation prompt. This is the feature that makes the whole thing a
workflow rather than a viewer.

`--all` must stay opt-in here as it is for `plan`, and an apply that would
destroy or replace anything should require a second, explicit confirmation.

### Baseline / diff-of-diffs
Fingerprint the sieved report (`Resource.ValueShape` is already the primitive)
and store it under `.tgsieve/baseline.json`. Then:

- `tgsieve plan --since-baseline` — show only what changed since the last
  accepted plan. This is the real answer to "a plan that has been noisy for
  three months and nobody reads any more".
- `tgsieve accept` — bless the current state as the baseline.
- exit code 2 only when the plan differs from the baseline, which turns a
  noisy CI check into a meaningful one.

### Output formats
- `--format json` — stable machine schema (the `sieve.Report` is already
  close); lets people build their own views without reparsing terraform.
- `--format md` — a PR comment: summary table, danger section, collapsed
  `<details>` for the rest.
- `--format sarif`-ish / annotations for CI systems that want inline warnings.

### Interactive TUI (`--tui`)
Bubbletea: unit list → resource list → attribute diff, `/` to filter, `h` to
toggle hidden attributes, `y` to yank an address. Only worth it once the static
view stops being where the time goes.

## Sieve improvements

- **Rule presets.** `extends: [builtin/aws-tags, builtin/k8s-annotations]` —
  curated starter rule sets people keep rewriting by hand. Must stay opt-in.
- **Set-aware paths.** Terraform renders sets as arrays, so reordering a set
  currently looks like N changed indices. Match set members by identity
  (hash of the element) instead of position.
- **Empty-vs-null normalization.** `"" → null` and `[] → null` are almost
  always noise; make it a `normalize:` config block rather than a hardcoded
  rule, so nobody is surprised.
- **Escaping in attribute paths.** A map key containing a `.` currently
  produces an ambiguous path. Quote such segments.
- **`--fail-on high|medium|low`.** Use the existing `severity:` config to fail
  a pipeline only on dangerous actions.
- **Per-rule expiry.** `expires: 2026-12-01` on a rule, warn once it lapses, so
  "temporary" suppressions do not become permanent blindness.
- **Drift section polish.** Separate "drift that the plan will revert" from
  "drift the plan ignores"; the second is what actually bites.

## Runner improvements

Done: queue size up front via `terragrunt find` (progress reads `7/28 planned ·
4 running`), `--timings` with per-unit durations and wall time, first-class
`--filter` / `--filter-affected` / `--parallelism`, graceful Ctrl-C (SIGINT is
forwarded so terraform can release its locks; whatever finished still renders,
the rest is reported as `NOT RUN`, exit 130), a plain heartbeat line for
non-tty runs, and resource-level progress for single-unit runs read from
terraform's `-json` stream.

Verified against OpenTofu 1.12.5 as well as Terraform 1.15.5: same plan
`format_version` (1.2), same keys, and the drift path was exercised with real
`resource_drift` data by deleting files a `local_file` resource owned. Two bugs
came out of that: single-unit runs double-counted the unit (terragrunt names it
by directory, we file the plan under its project-relative path), and skipped
units were counted as unchanged.

Also done: `--fast` (`-refresh=false`, with the summary stating that the plan
never looked at reality), root-cause grouping so one expired credential prints
once rather than once per unit, and `--resume`, which compares the queue
against the plans already in `--keep-plans` and runs only what is missing.

Also done: error grouping now folds incidental differences (resource
addresses, quoted and parenthesized identifiers, numbers, hashes) so one cause
reported N slightly different ways prints once, saying how many wordings it
covers; `--resume` records the commit plus a fingerprint of uncommitted changes
and refuses to mix generations without `--force`; and exit codes separate a
failed stack (3) from a failed tool (1), surviving changes (2) and an interrupt
(130).

Still open:

- **Provenance beyond git.** The generation check is git-only. A stack pulled
  from a `--source` URL, or a repo-less directory, records nothing and resume
  falls back to trusting the caller.
- **Concurrent runs.** Two `tgsieve plan --keep-plans` against the same
  directory will interleave their plans and overwrite each other's
  provenance. A lock file would make that a clear error.
- **Report timings for reused plans.** After `--resume` the timings section
  only covers units from the last invocation; the reused ones show nothing,
  which makes "slowest unit" misleading.

## Packaging

Done: `.goreleaser.yaml` (darwin/linux × amd64/arm64, archives, checksums,
Homebrew cask), release + CI workflows, version via ldflags.

`brew install imcitius/tap/tgsieve` works as of v0.1.0: the repository is
public, the release carries darwin/linux × amd64/arm64 archives, and
`imcitius/homebrew-tap` holds `Casks/tgsieve.rb`.

`HOMEBREW_TAP_TOKEN` is now set, so the next tag updates the cask on its own —
v0.1.0's cask was published by hand and is the last one that needs to be. The
first automated run is worth watching: if the token lacks `contents: write` on
the tap, goreleaser reports it at the very end of the release.

Also still open:

- GitHub Action wrapper that posts `--format md` as a PR comment.
- Signed / notarized macOS builds, so the cask does not need the quarantine
  hook.
