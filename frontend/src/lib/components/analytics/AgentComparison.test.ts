import { describe, expect, it } from "vite-plus/test";
import source from "./AgentComparison.svelte?raw";

describe("AgentComparison drilldowns", () => {
  it("preserves current session route params when navigating to agents", () => {
    expect(source).toContain("router.navigateToSessions({ agent: agent.name })");
    expect(source).not.toContain('router.navigate("sessions"');
  });
});
