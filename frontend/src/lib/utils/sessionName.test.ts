import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { visibleSessionName } from "./sessionName.js";
import { ui } from "../stores/ui.svelte.js";

describe("visibleSessionName", () => {
  beforeEach(() => {
    ui.setShowSessionNames(false);
  });

  afterEach(() => {
    ui.setShowSessionNames(false);
  });

  describe("absent or empty display_name", () => {
    it("returns null when display_name is absent", () => {
      expect(visibleSessionName({})).toBeNull();
    });

    it("returns null when display_name is null", () => {
      expect(visibleSessionName({ display_name: null })).toBeNull();
    });

    it("returns null when display_name is empty string", () => {
      expect(visibleSessionName({ display_name: "" })).toBeNull();
    });
  });

  describe("name_source === 'user' (manual rename)", () => {
    it("returns display_name when toggle is off", () => {
      expect(
        visibleSessionName({ display_name: "My Session", name_source: "user" }),
      ).toBe("My Session");
    });

    it("returns display_name when toggle is on", () => {
      ui.setShowSessionNames(true);
      expect(
        visibleSessionName({ display_name: "My Session", name_source: "user" }),
      ).toBe("My Session");
    });
  });

  describe("name_source === 'agent'", () => {
    it("returns null when toggle is off", () => {
      expect(
        visibleSessionName({
          display_name: "Agent Title",
          name_source: "agent",
        }),
      ).toBeNull();
    });

    it("returns display_name when toggle is on", () => {
      ui.setShowSessionNames(true);
      expect(
        visibleSessionName({
          display_name: "Agent Title",
          name_source: "agent",
        }),
      ).toBe("Agent Title");
    });
  });

  describe("name_source null/undefined (imported/legacy)", () => {
    it("returns display_name when name_source is null and toggle is off", () => {
      expect(
        visibleSessionName({ display_name: "Imported Title", name_source: null }),
      ).toBe("Imported Title");
    });

    it("returns display_name when name_source is absent and toggle is off", () => {
      expect(
        visibleSessionName({ display_name: "Legacy Title" }),
      ).toBe("Legacy Title");
    });

    it("returns display_name when name_source is null and toggle is on", () => {
      ui.setShowSessionNames(true);
      expect(
        visibleSessionName({ display_name: "Imported Title", name_source: null }),
      ).toBe("Imported Title");
    });
  });
});
