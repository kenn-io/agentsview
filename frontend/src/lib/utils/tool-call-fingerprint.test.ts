import { describe, expect, it } from "vite-plus/test";
import { toolCallFingerprint } from "./tool-call-fingerprint.js";

describe("toolCallFingerprint", () => {
  // The Go tests pin the same vectors, so a saved fingerprint means the same thing on both sides.
  it.each([
    ["Grep", '{"pattern":"retryLimit"}', "7805cd6e"],
    ["Read", '{"file_path":"界.go"}', "eee0ae21"],
    ["", "", "050c5d1f"],
  ])("fingerprints %s %s as the server does", (tool, input, want) => {
    expect(toolCallFingerprint(tool, input)).toBe(want);
  });

  it("tells apart calls that differ only in input or only in tool", () => {
    expect(toolCallFingerprint("Grep", '{"pattern":"a"}')).not.toBe(
      toolCallFingerprint("Grep", '{"pattern":"b"}'),
    );
    expect(toolCallFingerprint("Grep", "x")).not.toBe(toolCallFingerprint("Read", "x"));
  });
});
