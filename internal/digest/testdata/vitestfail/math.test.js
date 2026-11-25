import { describe, expect, it } from "vitest";

describe("math", () => {
  it("adds", () => {
    expect(1 + 1).toBe(2);
  });
  it("multiplies", () => {
    expect(2 * 2).toBe(5);
  });
  it.skip("divides", () => {});
});
