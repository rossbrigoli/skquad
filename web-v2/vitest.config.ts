import { defineConfig } from "vitest/config";

// Same philosophy as v1: unit-test the browser-independent logic layer.
export default defineConfig({
  test: {
    environment: "node",
    include: ["src/**/*.test.{ts,tsx}"],
    coverage: {
      provider: "v8",
      reporter: ["text", "lcov"],
      reportsDirectory: "coverage",
      include: ["src/lib/status.ts", "src/lib/format.ts"],
      thresholds: {
        statements: 85,
        lines: 85,
        functions: 85,
        branches: 90,
      },
    },
  },
});
