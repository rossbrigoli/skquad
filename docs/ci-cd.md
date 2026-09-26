# skquad CI/CD

> **Status:** Implemented validation CI, container image build/publish, and a
> lab GitOps deployment target with immutable image-tag promotion.

The GitHub Actions workflow in `.github/workflows/ci.yml` is the main quality
gate for the monorepo. It runs on pushes to `main` and on pull requests.

The image workflow in `.github/workflows/images.yml` builds deployable
containers for every charted component. Pull requests build the images without
publishing. Pushes to `main` and `v*.*.*` tags publish to GHCR. The workflow
also mirrors to Docker Hub when Docker Hub repository secrets are configured.

Since S-141 the workflow opens with a `version` job that computes the release's
semantic version (`major.minor.build`, build auto-incremented from the run
number) and stamps it on every component image plus `SKQUAD_VERSION` /
`SKQUAD_COMMIT` build args. See [versioning](versioning.md).

The lab deployment is managed from `/home/ross/projects/k3s-cluster` through
the ArgoCD Application `apps/app-skquad.yaml`. It tracks this repository's
`charts/skquad` path on `main`, deploys into `skquad-system`, pins the promoted
GHCR `sha-<short-sha>` image set, and keeps ingress disabled while the chart
still defaults to development authentication.

## Validation Jobs

| Job | Checks |
|-----|--------|
| Versioning | unit tests for the release-version computation (`major.minor.build`) and a `version.json` load check |
| Control plane | `go vet ./...` and `go test ./...` in `control-plane/` |
| Operator | `go vet ./...` and `go test ./...` in `operator/` |
| Agent runtime | installs the Python package, runs `unittest`, and compiles runtime/test modules |
| LLM gateway | validates Python project metadata/config, builds the gateway image, imports the Skquad LiteLLM callback inside the image, checks generated Prisma client artifacts against Postgres, and smoke-tests LiteLLM readiness against Postgres |
| Integration smoke | runs representative cross-component API, outbox, operator, runtime, and Helm contract checks from `tests/integration` |
| Helm chart | `helm lint`, default chart render, and provider-configured gateway render |
| Web | deterministic `npm ci` and `next build` |

## Current Boundaries

- Image-tag promotion is currently a deliberate GitOps commit to
  `/home/ross/projects/k3s-cluster/apps/app-skquad.yaml`; fully automated image
  updater wiring is still optional follow-up work.
- Kubernetes server-side dry-runs are still done locally against the lab
  cluster when a chart change needs API admission validation.
- Lab deployment promotion is considered complete only after ArgoCD applies the
  pinned `sha-<short-sha>` image set and the deployed workloads become healthy.
- The web job builds the current authenticated app shell plus first-pass squad,
  agent, task, registry, grant, admin, identity, and chat workflow UI. Rich
  charts, drag and drop, live updates, and guided onboarding are tracked
  separately.

## Image Publishing

Published images use the GHCR namespace `rossbrigoli`:

| Component | Image |
|-----------|-------|
| API server | `ghcr.io/rossbrigoli/skquad-api-server` |
| Operator | `ghcr.io/rossbrigoli/skquad-operator` |
| Agent runtime | `ghcr.io/rossbrigoli/skquad-agent-runtime` |
| LLM gateway | `ghcr.io/rossbrigoli/skquad-llm-gateway` |
| Web | `ghcr.io/rossbrigoli/skquad-web` |

Tags:

- `sha-<short-sha>` on pushes.
- `latest` on the default branch.
- `<semver>` and `<major>.<minor>` on `v*.*.*` tags.

Docker Hub mirroring uses the namespace `brigss007` and expects GitHub Actions
secrets:

- `DOCKERHUB_USERNAME`
- `DOCKERHUB_TOKEN`

## Follow-Up Work

- Add slower cluster-backed end-to-end tests for real agent pod execution and
  GitOps rollout once representative fixtures are available.
- Optionally replace manual GitOps promotion commits with ArgoCD Image Updater
  or a guarded workflow that opens promotion pull requests.
