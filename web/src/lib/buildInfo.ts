// S-141: build-time identity for the Skquad web UI.
//
// The release pipeline bakes SKQUAD_VERSION / SKQUAD_COMMIT into the image
// (Dockerfile ARG → build-stage env → next.config `env`, which inlines
// NEXT_PUBLIC_* into the client bundle at build time). The rail and the About
// page therefore show the exact version and commit of the bundle actually being
// served, without waiting on the control-plane API.
//
// Local `npm run dev` has no baked values and shows "unknown" by design — a
// dev build must never claim a release version it isn't.

export const UNKNOWN = "unknown";

function clean(value: string | undefined): string {
  const trimmed = (value ?? "").trim();
  return trimmed === "" ? UNKNOWN : trimmed;
}

export type BuildInfo = {
  readonly version: string;
  readonly commit: string;
  readonly shortCommit: string;
};

const version = clean(process.env.NEXT_PUBLIC_SKQUAD_VERSION);
const commit = clean(process.env.NEXT_PUBLIC_SKQUAD_COMMIT);

export const buildInfo: BuildInfo = {
  version,
  commit,
  shortCommit: commit === UNKNOWN ? UNKNOWN : commit.slice(0, 7),
};

/** "v0.1.100" — what the left rail shows under the wordmark. */
export const versionLabel = `v${version}`;

/** "0.1.100 (commit 4ae923c)" — what the About page shows. */
export const buildLabel = `${version} (${commit === UNKNOWN ? "commit unknown" : `commit ${buildInfo.shortCommit}`})`;

/** Display guard for API-supplied version strings: blank/whitespace → "unknown". */
export function versionText(value: string | undefined): string {
  return clean(value);
}
