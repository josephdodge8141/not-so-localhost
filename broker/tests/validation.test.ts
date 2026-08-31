import { describe, expect, it } from "vitest";
import { appHostname, parseEnrollmentToken, requireSlug, slugify } from "../src/validation";

describe("broker validation", () => {
  it("normalizes display names", () => {
    expect(slugify("Laptop Three")).toBe("laptop-three");
  });

  it("requires canonical API slugs", () => {
    expect(requireSlug("laptop-three", "node_name")).toBe("laptop-three");
    expect(() => requireSlug("Laptop Three", "node_name")).toThrow(/lowercase DNS label/);
  });

  it("parses enrollment tokens without weakening their shape", () => {
    expect(parseEnrollmentToken("nsl1.id.secret")).toEqual({ id: "id", secret: "secret" });
    expect(() => parseEnrollmentToken("secret")).toThrow(/invalid/);
  });

  it("uses a flat hostname covered by Universal SSL", () => {
    expect(appHostname("litellm", "laptop-three", "joedodge.dev")).toBe("litellm--laptop-three.joedodge.dev");
  });

  it("reserves the terminal hostname", () => {
    expect(() => appHostname("t", "laptop-three", "joedodge.dev")).toThrow(/reserved/);
  });

  it("rejects a combined DNS label over 63 characters", () => {
    expect(() => appHostname("a".repeat(40), "b".repeat(30), "joedodge.dev")).toThrow(/DNS label/);
  });
});
