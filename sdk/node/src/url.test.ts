import { describe, expect, it } from "vitest";
import { stripTrailingSlashes } from "./url.js";

describe("stripTrailingSlashes", () => {
  it("matches the pattern it replaced on ordinary inputs", () => {
    const inputs = [
      "",
      "/",
      "///",
      "http://muninn-test",
      "http://muninn-test/",
      "http://muninn-test///",
      "http://muninn-test/base/",
      "http://muninn-test//base",
      "no-slash",
      "a/b/c",
    ];
    for (const input of inputs) {
      expect(stripTrailingSlashes(input)).toBe(input.replace(/\/+$/, ""));
    }
  });

  it("is linear on a long run of slashes that is not at the end", () => {
    // The pattern /\/+$/ needs on the order of n^2 steps here (seconds at this
    // size); the loop stops at the last character.
    const input = "/".repeat(200_000) + "x";
    const start = performance.now();
    const out = stripTrailingSlashes(input);
    const elapsedMs = performance.now() - start;
    expect(out).toBe(input);
    expect(elapsedMs).toBeLessThan(250);
  });
});
