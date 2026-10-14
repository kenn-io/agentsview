import { describe, expect, it } from "vite-plus/test";
import {
  getAlignedOffsetScrollAlign,
  getLatestDisplayIndex,
  isAtLatestEdge,
} from "./message-scroll.js";

describe("message scroll helpers", () => {
  it("targets the last rendered row when oldest messages are first", () => {
    expect(getLatestDisplayIndex(5, false)).toBe(4);
  });

  it("targets the first rendered row when newest messages are first", () => {
    expect(getLatestDisplayIndex(5, true)).toBe(0);
  });

  it("detects the latest edge at the bottom in oldest-first order", () => {
    expect(isAtLatestEdge({ scrollTop: 395, scrollHeight: 500, clientHeight: 100 }, false)).toBe(
      true,
    );
    expect(isAtLatestEdge({ scrollTop: 300, scrollHeight: 500, clientHeight: 100 }, false)).toBe(
      false,
    );
  });

  it("detects the latest edge at the top in newest-first order", () => {
    expect(isAtLatestEdge({ scrollTop: 3, scrollHeight: 500, clientHeight: 100 }, true)).toBe(true);
    expect(isAtLatestEdge({ scrollTop: 20, scrollHeight: 500, clientHeight: 100 }, true)).toBe(
      false,
    );
  });

  it("uses direct offset semantics for already-aligned offsets", () => {
    expect(getAlignedOffsetScrollAlign("start")).toBe("start");
    expect(getAlignedOffsetScrollAlign("end")).toBe("start");
  });
});
