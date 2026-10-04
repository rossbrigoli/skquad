// S-189: shared setup for the jsdom project. jest-dom matchers + explicit
// RTL cleanup (the suite runs without vitest globals, so RTL cannot
// auto-register its afterEach).
import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

afterEach(() => {
  cleanup();
});

// jsdom does not implement the native <dialog> methods that Modal relies
// on (showModal/close). Minimal shim so dialogs mount and unmount.
if (typeof window.HTMLDialogElement !== "undefined") {
  const proto = window.HTMLDialogElement.prototype as unknown as Record<string, unknown>;
  if (!proto.showModal) {
    proto.showModal = function (this: HTMLDialogElement) {
      this.open = true;
    };
  }
  if (!proto.show) {
    proto.show = function (this: HTMLDialogElement) {
      this.open = true;
    };
  }
  if (!proto.close) {
    proto.close = function (this: HTMLDialogElement) {
      this.open = false;
    };
  }
}
