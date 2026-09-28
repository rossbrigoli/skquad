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
      include: ["src/lib/status.ts", "src/lib/format.ts", "src/lib/attention.ts", "src/lib/agentStorage.ts", "src/components/MarkdownMessage.tsx"],
      thresholds: {
        statements: 85,
        lines: 85,
        functions: 85,
        branches: 90,
      },
    },
  },
});
