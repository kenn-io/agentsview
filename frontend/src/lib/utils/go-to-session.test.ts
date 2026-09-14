import { describe, expect, it } from "vite-plus/test";
import { resolveSessionId, sessionLookupPartial } from "./go-to-session.js";

const UUID = "123e4567-e89b-12d3-a456-426614174000";

describe("resolveSessionId", () => {
  it("trims the input and preserves a local canonical ID", () => {
    expect(resolveSessionId(`  ${UUID}  `, [`codex:${UUID}`], false)).toEqual({
      kind: "resolved",
      id: `codex:${UUID}`,
    });
  });

  it("resolves a remote canonical ID with a host delimiter", () => {
    const id = `build-host~codex:${UUID}`;
    expect(resolveSessionId(UUID, [id], false)).toEqual({ kind: "resolved", id });
  });

  it("resolves an opaque session ID", () => {
    const id = "test-session-project-reclassification-nested";
    expect(resolveSessionId(id, [id], false)).toEqual({ kind: "resolved", id });
  });

  it("treats a matching opaque ID as exact among partial results", () => {
    const id = "session-name-123";
    expect(resolveSessionId(id, ["session-name-1234", id], false)).toEqual({
      kind: "resolved",
      id,
    });
  });

  it("reports an empty value", () => {
    expect(resolveSessionId("", [`codex:${UUID}`], false)).toEqual({ kind: "empty" });
  });

  it("preserves opaque ID casing while normalizing bare UUID lookup", () => {
    expect(sessionLookupPartial(" test-session-ABC ")).toBe("test-session-ABC");
    expect(sessionLookupPartial(` ${UUID.toUpperCase()} `)).toBe(UUID);
  });

  it("reports an unknown UUID when no canonical candidate matches", () => {
    expect(resolveSessionId(UUID, ["codex:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"], false)).toEqual(
      { kind: "unknown" },
    );
  });

  it("does not match a UUID that only appears inside a host name", () => {
    expect(resolveSessionId(UUID, [`${UUID}.example.test~codex:other`], false)).toEqual({
      kind: "unknown",
    });
  });

  it("deduplicates repeated canonical IDs", () => {
    const id = `claude:${UUID}`;
    expect(resolveSessionId(UUID, [id, id, id], false)).toEqual({ kind: "resolved", id });
  });

  it("keeps duplicate provider suffixes ambiguous", () => {
    expect(
      resolveSessionId(UUID, [`claude:${UUID}`, `host~codex:${UUID}`], false),
    ).toEqual({ kind: "ambiguous" });
  });

  it("does not resolve a capped response at the 1000-result boundary", () => {
    const candidates = Array.from({ length: 999 }, (_, index) => `other-${index}`);
    candidates.push(`codex:${UUID}`);
    expect(resolveSessionId(UUID, candidates, true)).toEqual({ kind: "capped" });
  });
});
