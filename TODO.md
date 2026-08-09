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

- **Unit count up front.** `terragrunt find --format json` before the run to
  turn the progress line into `7/28` instead of "7 seen".
- **Per-unit timing in the report.** The run report already carries durations;
  surface the slowest units under `--timings`.
- **`--filter-affected` as a first-class flag** rather than via `--tg-args`,
  plus `--filter` passthrough.
- **Cancellation.** Ctrl-C currently kills terragrunt via context; make sure
  partial plans still render instead of being discarded.
- **OpenTofu.** `resolveTFPath` prefers tofu when present; test against tofu's
  plan JSON, which has diverged slightly (e.g. `resource_drift` details).
- **Non-tty progress.** Emit a plain line every N seconds so CI logs show
  liveness without the spinner.

## Packaging

Done: `.goreleaser.yaml` (darwin/linux × amd64/arm64, archives, checksums,
Homebrew cask), release + CI workflows, version via ldflags.

Left to do before `brew install imcitius/tap/tgsieve` works:

1. Create the public tap repo `imcitius/homebrew-tap` (a bare repo with a
   `Casks/` directory is enough; goreleaser commits the file).
2. Add a `HOMEBREW_TAP_TOKEN` secret to this repo — a fine-grained PAT with
   `contents: write` on the tap repo. Without it the release still publishes,
   only the cask update is skipped.
3. Make this repository public. Homebrew downloads release assets
   unauthenticated, so a tap pointing at a private repo cannot install.
4. Tag a release: `git tag v0.1.0 && git push --tags`.

Also still open:

- GitHub Action wrapper that posts `--format md` as a PR comment.
- Signed / notarized macOS builds, so the cask does not need the quarantine
  hook.
