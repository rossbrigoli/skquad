# Skquad Versioning (S-141)

Skquad uses semantic versioning in the shape **`major.minor.build`** (e.g. `0.1.100`).
Every component in a release carries the same version, and the build number
auto-increments on every release — nobody edits a version file by hand, and CI
never commits back to git.

## Where the numbers live

| Concern | Source of truth |
|---|---|
| `major`, `minor` | `version.json` (repo root) — edited by hand when you intentionally bump |
| `build` | Derived per release: `build = buildBase + (releaseRunNumber - runBase)` |
| Commit hash | The SHA the release pipeline checked out (`github.sha`) |
| Baseline (`buildBase`, `runBase`) | `version.json` — the last already-released (run, build) pair |

`version.json` as it stands:

```json
{ "major": 0, "minor": 1, "buildBase": 99, "runBase": 147 }
```

The baseline means: *Images run 147 already produced build 99*. So the next
release run (148) is **0.1.100**, the one after (149) is **0.1.101**, and so on.

## How the pipeline does it

`.github/actions/skquad-version` (composite action + `compute_version.py`) turns
`(version.json, run_number, commit)` into outputs `version`, `version_tag`,
`build`, `commit`, `commit_short`.

`images.yml` (the release pipeline) runs it once in the `version` job, then every
component image is tagged with that version:

```
ghcr.io/rossbrigoli/skquad-api-server:0.1.100     (+ :0.1, :sha-<sha>, :latest)
ghcr.io/rossbrigoli/skquad-operator:0.1.100
ghcr.io/rossbrigoli/skquad-agent-runtime:0.1.100
ghcr.io/rossbrigoli/skquad-llm-gateway:0.1.100
ghcr.io/rossbrigoli/skquad-web:0.1.100
```

and passes `SKQUAD_VERSION` / `SKQUAD_COMMIT` as Docker build args.

**Why the Images workflow, not the test workflow, owns the counter:** the build
number only advances when something is actually published, so released versions
have no gaps. PR runs execute the same code under test (`ci.yml` → `versioning`
job) but do not consume build numbers. If you would rather every CI run burned a
number, feed the CI `run_number` into the same action instead — the math is
identical.

**Monotonicity guard:** `compute_version.py` exits 1 if `run_number <= runBase`.
Replaying an older pipeline run cannot publish a *lower* version than what is
already out there.

## How it reaches the UI

- **Web UI** bakes both values at build time: Dockerfile `ARG` → `SKQUAD_VERSION` /
  `SKQUAD_COMMIT` env → `next.config.ts` `env` inlines `NEXT_PUBLIC_SKQUAD_VERSION` /
  `NEXT_PUBLIC_SKQUAD_COMMIT` into the client bundle. `web/src/lib/buildInfo.ts` is
  the single accessor. A local `npm run dev` shows `unknown` on purpose — a dev
  build must not claim a release version.
- **Other components** keep the existing S-130 path: the Helm chart maps the deployed
  image tag to `SKQUAD_*_VERSION`, and `GET /api/v1/versions` returns them.
- **Commit hash** flows `git.commit` (chart values) → `SKQUAD_GIT_COMMIT` →
  `GET /api/v1/versions` `commit` field.

### Displayed

- **Left rail**, directly under the `skquad` wordmark: `v0.1.100` (the UI's own
  baked version).
- **About page**: the release version + commit under the heading, plus the
  per-component version table from `/api/v1/versions`.

## Bumping major/minor

1. Edit `version.json`: set `major`/`minor`, set `buildBase` to `0` (or `1`), and
   set `runBase` to the current latest Images run number.
2. Commit and merge. The next release is `<major>.<minor>.1` (or `.0`).

Never lower a published version. The guard in `compute_version.py` will refuse a
replay rather than silently go backwards.

## Tests

```bash
# version math
python3 -m unittest discover -s .github/actions/skquad-version -p 'test_*.py'
# UI identity (baked values, fallbacks, labels)
cd web && npx vitest run src/lib/buildInfo.test.ts
# API surface (commit field)
cd control-plane && go test ./internal/httpapi/ ./internal/config/
```
