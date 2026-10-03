import { defineConfig } from "vitest/config";

// Same philosophy as v1: unit-test the browser-independent logic layer.
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.{ts,tsx}"],
    // S-171: junit output feeds SonarQube test-execution metrics
    // (converted by scripts/testreport-to-sonar.py in the security-sonar job).
    reporters: ["default", "junit"],
    outputFile: { junit: "coverage/junit.xml" },
    coverage: {
      provider: "v8",
      reporter: ["text", "lcov"],
      reportsDirectory: "coverage",
      // S-182: widened from the original 5-file allowlist to the whole logic
      // layer. Almost every lib module already has a colocated *.test.ts; the
      // stale include list was hiding them from lcov, which is why SonarQube
      // reported web at ~10% despite the tests existing.
      include: [
        "src/lib/**/*.ts",
        "src/components/MarkdownMessage.tsx",
        "src/components/AgentTiles.tsx",
      ],
      exclude: [
        "src/**/*.test.ts",
        "src/**/*.test.tsx",
        // React client hooks need a DOM test environment (jsdom) which this
        // node-env suite deliberately does not load; they are thin wrappers
        // over apiGet/useState. DOM coverage is a separate follow-up.
        "src/lib/useApi.ts",
        "src/lib/usePromptValidation.ts",
      ],
      thresholds: {
        statements: 85,
        lines: 85,
        functions: 85,
        branches: 90,
      },
    },
  },
});
