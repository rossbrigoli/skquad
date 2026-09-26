import type { NextConfig } from "next";

// S-141: bake the release version + commit into the bundle. The Images workflow
// passes SKQUAD_VERSION / SKQUAD_COMMIT as Docker build args; `next.env` inlines
// NEXT_PUBLIC_* values into the client bundle at build time, so the rail and the
// About page show the identity of this exact bundle. A plain `next build` outside
// the release pipeline leaves them empty and the UI shows "unknown".
const nextConfig: NextConfig = {
  env: {
    NEXT_PUBLIC_SKQUAD_VERSION: process.env.SKQUAD_VERSION ?? "",
    NEXT_PUBLIC_SKQUAD_COMMIT: process.env.SKQUAD_COMMIT ?? "",
  },
};

export default nextConfig;
