import { defineConfig } from "vitest/config";

// Unit tests target the browser-independent logic layer: the API client and the
// pure helpers used by the components. Component rendering is intentionally out
// of scope for now (it needs a jsdom + testing-library setup), so coverage is
// scoped to the modules that actually hold decision logic.
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.{ts,tsx}"],
    coverage: {
      provider: "v8",
      reporter: ["text", "lcov"],
      reportsDirectory: "coverage",
      include: ["src/lib/api.ts", "src/components/shared.tsx"],
      // Ratchet floor: raise it whenever the logic layer gains tests, never
      // let it drift down. Current actuals are ~92% statements / 100% branches.
      thresholds: {
        statements: 85,
        lines: 85,
        functions: 85,
        branches: 90,
      },
    },
  },
});
