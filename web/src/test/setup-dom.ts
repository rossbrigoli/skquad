// S-189: shared setup for the jsdom project. jest-dom matchers + explicit
// RTL cleanup (the suite runs without vitest globals, so RTL cannot
// auto-register its afterEach).
import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

afterEach(() => {
  cleanup();
});
