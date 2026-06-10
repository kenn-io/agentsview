import { describe, it, expect, vi } from "vite-plus/test";
import { copyToClipboard } from "./clipboard.js";

describe("copyToClipboard", () => {
  it("copies text and returns true on success", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    const result = await copyToClipboard("test-id");
    expect(writeText).toHaveBeenCalledWith("test-id");
    expect(result).toBe(true);
    vi.unstubAllGlobals();
  });

  it("returns false when clipboard write fails", async () => {
    const writeText = vi.fn().mockRejectedValue(new Error("denied"));
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    const result = await copyToClipboard("test-id");
    expect(result).toBe(false);
    vi.unstubAllGlobals();
  });
});
