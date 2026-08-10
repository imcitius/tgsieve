# TODO

v1 shipped: terragrunt runner (json-out-dir + json log stream + run report),
plan parsing with attribute-level diffs, noise rules, collapsing, tty renderer,
`plan` / `show` / `rules` / `init`.

## Next up

### Applying — shipped, with gaps

`tgsieve apply` plans, renders, asks and applies the saved plan files, with
`--plans` to apply a review from earlier and the generation guard refusing
plans made against code that has since moved. `--all` stays opt-in, destroys
and replacements need their own confirmation, and a run without a terminal
refuses unless `--auto-approve` says otherwise.

Two bugs found in v0.4.0 and fixed in v0.4.1: terragrunt asks its own "are you
sure" for a stack apply, and the child process has no terminal, so it read EOF
and aborted — while the outcome block still announced APPLIED. Runs tgsieve
drives now pass --non-interactive (tgsieve does the asking itself), and the
outcome refuses to describe a failed run as applied.

Still open there:

- **No per-resource progress.** The status line counts units, so a single unit
  creating forty things looks stalled. terraform's `-json` stream carries
  apply events; a stack run cannot use it (the lines interleave unlabelled),
  but a single-unit apply could.
- **A failed apply reports units, not resources.** When a unit fails halfway,
  the report still describes what was planned. What actually landed is only
  discoverable by planning again.
- **No output values after apply.** Terraform prints them; tgsieve currently
  swallows them along with everything else.
- **`--plans` does not verify the binary plans match the JSON ones** it renders
  from. They are written by the same run today, so this is only a concern if
  someone edits the directory.

### Baseline / diff-of-diffs
Fingerprint the sieved report (`Resource.ValueShape` is already the primitive)
and store it under `.tgsieve/baseline.json`. Then:

- `tgsieve plan --since-baseline` — show only what changed since the last
  accepted plan. This is the real answer to "a plan that has been noisy for
  three months and nobody reads any more".
- `tgsieve accept` — bless the current state as the baseline.
- exit code 2 only when the plan differs from the baseline, which turns a
  noisy CI check into a meaningful one.

### Output formats — done

`--format` covers `tty`, `md`, `json` and `github`. Markdown folds everything
but destruction, carries a `<!-- tgsieve -->` marker so a bot can update its
own comment, and reports the apply outcome too. JSON is a versioned document
with its own types, and omits values terraform marked sensitive. GitHub emits
workflow commands, one annotation per location a failure came from.

Still open:

- **SARIF.** Considered and skipped: SARIF describes static analysis findings
  in source, and a plan is a statement about infrastructure. Failures would map
  (they have file and line); planned changes would not, and half a format is
  worse than none.
- **`$GITHUB_STEP_SUMMARY`.** `--format md >> "$GITHUB_STEP_SUMMARY"` already
  works; writing it directly would save a shell redirect and nothing else.
- **Schema documentation.** The JSON shape is described by example in the
  README and pinned by tests, but there is no published schema file.

### Interactive TUI (`--tui`)
Bubbletea: unit list → resource list → attribute diff, `/` to filter, `h` to
toggle hidden attributes, `y` to yank an address. Only worth it once the static
view stops being where the time goes.

## Engines

`--engine terraform` drives terraform or tofu directly for a single root
module, with the same sieve, collapsing, formats and apply flow. Still open:

- **Several root modules in one run.** The terragrunt engine has a queue; the
  terraform one plans exactly one directory. Walking a tree of root modules
  would need its own ordering and dependency story, which is most of what
  terragrunt exists for.
- **Workspaces.** `terraform workspace` is invisible to the report; two
  workspaces of one module render identically.
- **Provenance.** The generation guard fingerprints configuration and git, but
  a direct run records no module sources, since there is no terragrunt config
  to resolve them from.

## Sieve improvements

Done: arrays are compared by membership rather than position (a reordered set
is one line, an element leaving the middle no longer shifts every index into a
false change), a `normalize:` block for empty-versus-null and reorderings,
quoted path segments for keys containing dots (and attribute globs that cover
both spellings), built-in rule presets via `extends`, `--fail-on` backed by
per-action severity, per-rule `expires` that fails open and says so, drift
split by whether the plan addresses it, and identity-matched objects inside
collections.

Note on coverage: the "this plan leaves it" drift branch is unit-tested but has
never been seen on a real plan here, because no provider available offline
produces that shape — `local_file` keys on its content hash, so editing a file
reads as the resource being gone rather than as an ignored attribute. Worth
confirming against a cloud provider with `ignore_changes` before trusting the
wording.


- **Identity fields are a fixed list.** `id`, `name`, `key`, `cidr_block` and
  friends cover common shapes, but a collection keyed by something else falls
  back to membership. A per-type identity hint in config would generalize it.
- **Preset drift.** Presets are a fixed list compiled into the binary. A
  project that wants one changed has to fork it; `extends` accepting a path or
  URL would help, but turns "what is hidden" into something fetched at runtime.
- **Severity for attributes, not just actions.** `--fail-on` ranks by action,
  so an update to a security group rule and an update to a description are
  equally medium. Ranking by attribute would need a rule syntax of its own.

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

Also done: provenance outside git (the configuration files are fingerprinted
when there is no repository), a lock on the `--keep-plans` directory so two
runs cannot interleave plans (stale locks are taken over, live ones reported),
and per-unit durations remembered across invocations so `--timings` covers the
whole stack after a resume, marking reused plans.

Also done: resolved `terraform { source }` per unit recorded in provenance
(rendered per unit in parallel, because `render --all` writes its objects
unlabelled), cross-host lock handling (respected for six hours, then taken
over, since pid liveness means nothing on another machine), and a two-week TTL
on remembered durations with the age shown next to reused measurements.

Also done: floating refs resolved to commits via `git ls-remote` (cached per
repository and ref, `--no-resolve-refs` to disable, unreachable remotes degrade
rather than fail), `.terragrunt-stack` units folded into the fingerprint, and
`--lock-wait` for CI pipelines racing on one plan directory.

The runner list is empty. What follows are the items that would justify
reopening it:

- **Provenance is advisory, not enforced.** `--force` still lets anyone mix
  generations, and nothing stops a plan file being edited by hand. If plans
  ever travel between machines — CI plans applied from a laptop — they should
  be signed rather than merely described.
- **`ls-remote` on every run.** Cheap for a handful of modules, wasteful for a
  stack drawing on dozens. A short-lived cache keyed by repository and ref
  would fix it; the current cache lives only for one run.

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
