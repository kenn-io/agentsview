import { describe, expect, it } from "vite-plus/test";
import { resolveSessionId } from "./go-to-session.js";

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

  it.each([
    ["", "empty"],
    ["123e4567-e89b-12d3-a456-42661417400", "invalid"],
    [`${UUID.slice(0, 8)} ${UUID.slice(9)}`, "invalid"],
    [`codex:${UUID}`, "invalid"],
    [`https://example.test/sessions/${UUID}`, "invalid"],
    ["123e4567-e89b-12d3-a456-42661417400z", "invalid"],
  ] as const)("rejects %j as %s", (input, kind) => {
    expect(resolveSessionId(input, [`codex:${UUID}`], false)).toEqual({ kind });
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
