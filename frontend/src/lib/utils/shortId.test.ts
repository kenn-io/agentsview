import { describe, expect, it } from "vite-plus/test";
import { shortenId } from "./shortId.js";

describe("shortenId", () => {
  it.each<[string, string[], string]>([
    ["", [], ""],
    ["job-a", ["job-b"], "job-a"],
    ["12345678-first", ["12345678-second"], "…78-first"],
    ["12345678-middle-abcdefgh", ["12345678-other-abcdefgh"], "12345678-middle-abcdefgh"],
  ])("disambiguates %s", (id, peers, expected) => {
    expect(shortenId(id, peers)).toBe(expected);
  });
});
