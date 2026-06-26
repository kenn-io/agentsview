import { describe, expect, it } from "vite-plus/test";
import source from "./SessionShape.svelte?raw";

describe("SessionShape drilldowns", () => {
  it("preserves current session route params when navigating to buckets", () => {
    expect(source).toContain(
      'router.navigateToSessions(params, ["min_messages", "max_messages"])',
    );
    expect(source).not.toContain('router.navigate("sessions", params)');
  });
});
