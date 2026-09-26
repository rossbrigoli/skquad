"use client";

// S-130: About page — banner, licence, and the version of every Skquad
// component. Versions come from GET /api/v1/versions (env-driven, filled
// by the Helm chart from the deployed image tags). Accessible to all
// roles; the shell already requires auth.

import Image from "next/image";
import { AuthGate } from "../../components/AuthGate";
import { AppShell } from "../../components/AppShell";
import { useApi } from "../../lib/useApi";

type Versions = {
  readonly api_server?: string;
  readonly operator?: string;
  readonly agent_runtime?: string;
  readonly llm_gateway?: string;
  readonly web_ui?: string;
};

const VERSION_ROWS: readonly { readonly key: keyof Versions; readonly label: string }[] = [
  { key: "operator", label: "Skquad Kubernetes Operator" },
  { key: "web_ui", label: "Skquad UI" },
  { key: "api_server", label: "Skquad API server" },
  { key: "agent_runtime", label: "Skquad Agent Runtime" },
  { key: "llm_gateway", label: "Skquad LLM Gateway" },
];

function versionText(value: string | undefined): string {
  return value && value.trim() !== "" ? value : "unknown";
}

export default function AboutPage() {
  const versions = useApi<Versions>("/versions");

  return (
    <AuthGate>
      <AppShell>
        <section className="about-page">
          <div className="about-banner">
            {/* Theme-aware banner: transparent artwork on light, white
                artwork on dark. CSS picks one via [data-theme]. */}
            <Image
              src="/skquad-transparent.png"
              alt="Skquad"
              className="about-banner-img about-banner-light"
              width={1448}
              height={1086}
              priority
            />
            <Image
              src="/skquad-white.png"
              alt="Skquad"
              className="about-banner-img about-banner-dark"
              width={2048}
              height={682}
              priority
            />
          </div>

          <h1 className="about-title">About Skquad</h1>

          <p className="about-license">
            Skquad is open source software licensed under the{" "}
            <a href="https://www.apache.org/licenses/LICENSE-2.0" target="_blank" rel="noopener noreferrer">
              Apache License, Version 2.0
            </a>
            .
          </p>

          <h2 className="about-versions-heading">Component versions</h2>
          {versions.loading ? (
            <p className="about-muted">loading versions…</p>
          ) : versions.error ? (
            <p className="about-muted">versions unavailable: {versions.error}</p>
          ) : (
            <table className="about-versions">
              <tbody>
                {VERSION_ROWS.map((row) => (
                  <tr key={row.key}>
                    <th scope="row">{row.label}</th>
                    <td className="about-version-value">{versionText(versions.data?.[row.key])}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </section>
      </AppShell>
    </AuthGate>
  );
}
