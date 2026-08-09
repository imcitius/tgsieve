# tgsieve

Run `terragrunt run --all -- plan` over a heavy stack and you get thousands of
lines of terraform prose, in which the two changes you care about are somewhere
between a tag timestamp and the ninth identical `count` instance.

`tgsieve` runs the plan for you, reads the **structured** output instead of the
prose, throws away the noise you declared as noise, collapses everything that
repeats, and prints what is left.

```
DESTROY / REPLACE (3)
  ± envs/dev/b  null_resource.pin
      id               "972339063827744647" → (known after apply)
      triggers.region  "eu-west-1" → "us-east-1"  forces replacement
  - envs/prod/c  terraform_data.extra[0-1] ×2
      3 attributes (-v to show)

UPDATE (5)
  ~ 5 units  terraform_data.cfg ×5
      in envs/dev/a, envs/dev/b, envs/prod/a, envs/prod/b, envs/prod/c
      input.size  "small" → "large"
      (2 attributes hidden by rules)

SUMMARY  ±1 replace  -2 destroy  ~5 update
  5 units · 5 with changes · 0 unchanged · 0 failed
  sieved: 10 attributes and 0 resources hidden by 2 rules (--explain)
```

## How it works

There is no screen-scraping. `tgsieve` asks terragrunt for machine-readable
artifacts and reads those:

| What | Flag it passes | What it gets |
| --- | --- | --- |
| per-unit plans | `--json-out-dir` | one `tfplan.json` per unit — the full `terraform show -json` document |
| live progress | `--log-format json` | NDJSON events tagged with `working-dir`, so failures surface the moment they happen |
| run report | `--report-file/--report-format json` | per-unit result and duration, including units that failed before producing a plan |

From each plan it computes a real attribute-level diff — flattening
`before`/`after`/`after_unknown` into dotted paths, honouring
`replace_paths`, sensitivity, and unknown subtrees.

Two terragrunt details this works around:

- `terragrunt run --all -- plan -json` is **not** usable. Terragrunt forwards
  terraform's own NDJSON straight through, so lines from units running in
  parallel interleave with no way to tell them apart.
- `--json-out-dir` and `--out-dir` only apply to `--all` runs. For a
  single unit `tgsieve` drives it itself: plan to a binary file, then
  `terragrunt run -- show -json <file>` to get the same document.

## Install

```bash
brew install imcitius/tap/tgsieve
```

```bash
go install github.com/imcitius/tgsieve@latest
```

or from a checkout: `go build -o tgsieve .`

## Use

```bash
tgsieve plan                                  # only the unit in the working directory
tgsieve plan --all                            # the whole stack below the working directory
tgsieve plan -C envs/prod --all -- -refresh=false   # args after -- go to terraform
tgsieve plan --all --keep-plans ./plans       # keep the per-unit tfplan.json
tgsieve show ./plans --explain                # re-render, no re-run
tgsieve plan --all --detailed-exitcode        # 0 = nothing survived the sieve, 2 = something did
tgsieve rules                                 # what config is in effect, and from where
tgsieve init                                  # write a starter .tgsieve.yaml at the project root
```

**The stack is never planned implicitly.** `--all` (`-a`) is opt-in, so a
mistyped command touches one unit rather than the whole estate — the same
default will hold for `apply` when it lands.

Useful flags: `-v` (attributes for creates and destroys too), `--show-empty`,
`--explain`, `--no-sieve`, `--no-color`, `--max-attrs`, `--max-units`,
`--out-dir` (keep binary plans so you can apply exactly what you reviewed),
`--tg-args "--filter-affected"` (pass extra flags to terragrunt itself).

Exit codes: `0` fine, `1` a unit failed, `2` changes survived the sieve (only
with `--detailed-exitcode`).

## Noise rules

`tgsieve init` writes a starter file at the project root — the git root, or
failing that the highest directory that still holds a terragrunt root config
(`--here` writes into the current directory instead, `-C <dir>` anywhere).

Nothing is hidden until you say so. `.tgsieve.yaml` is looked up from the
working directory upwards, stopping at the repo root; every file found is
merged, nearer files winning.

```yaml
version: 1

hide:
  unchanged_units: true   # units with nothing left to say become a count
  drift: false            # refresh-detected drift gets its own section
  outputs: false

ignore:
  - name: tag churn
    attrs: ["tags.LastModified", "tags.git_commit", "tags_all.*"]
  - name: ecs task revision
    type: aws_ecs_task_definition
    attrs: ["revision"]
  - name: dev is not interesting
    unit: "envs/dev/**"
    attrs: ["*"]

never_hide:
  actions: [delete, replace]
  types: []

collapse:
  instances: true
  cross_unit: true
  cross_unit_mode: shape   # "shape" ignores values, "strict" requires equal ones
  min_units: 2
```

A rule matches a resource when every selector it sets matches (`unit`, `type`,
`address`, `actions`), and then removes the attributes matching `attrs`.
**`attrs` is required** — use `["*"]` to drop the whole resource.

Globs: `*` matches anything except `/` (so it crosses `.` inside attribute
paths), `**` matches anything, `?` matches one non-`/` character.

### Why you can trust it

- **A resource only disappears when every one of its attributes was hidden.**
  One surviving attribute keeps the resource on screen.
- **`never_hide` wins over every rule.** By default destroys and replacements
  can never be silenced.
- **An attribute that forces replacement is never hidden**, whatever the rules
  say.
- **The counts stay honest.** `×12` and the summary count real resources, not
  rendered blocks.
- **`--explain` shows every hidden attribute and the rule that hid it**, and
  the footer always states how much was hidden.

## Collapsing

- `foo[0]`, `foo[1]`, `foo[2]` with the same diff → one row `foo[0-2] ×3`.
- the same change in many units → one block listing the units.
- values that agree across the group are printed; only the ones that actually
  differ are shown as `(varies by unit)`.

## Development

```bash
go test ./...
```

Releases are cut by tagging: `git tag v0.1.0 && git push --tags` runs
goreleaser, which publishes archives for darwin/linux × amd64/arm64 and updates
the Homebrew cask in `imcitius/homebrew-tap`. Validate changes to the release
config with `goreleaser check` and rehearse with
`goreleaser release --snapshot --clean --skip=publish`.

See [TODO.md](TODO.md) for what is planned next.
