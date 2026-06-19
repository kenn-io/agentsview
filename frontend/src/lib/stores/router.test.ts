import {
  describe,
  it,
  expect,
  vi,
  afterEach,
} from "vite-plus/test";
import {
  parsePath,
  RouterStore,
} from "./router.svelte.js";

function setURL(path: string) {
  window.history.replaceState(null, "", path);
}

describe("parsePath", () => {
  afterEach(() => {
    setURL("/");
  });

  it("returns default route for root path", () => {
    setURL("/");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBeNull();
    expect(result.params).toEqual({});
  });

  it("parses /sessions with query params", () => {
    setURL("/sessions?project=myproj&machine=laptop");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBeNull();
    expect(result.params).toEqual({
      project: "myproj",
      machine: "laptop",
    });
  });

  it("parses /sessions/{id}", () => {
    setURL("/sessions/abc-123");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBe("abc-123");
    expect(result.params).toEqual({});
  });

  it("parses /sessions/{id} with msg param", () => {
    setURL("/sessions/abc-123?msg=5");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBe("abc-123");
    expect(result.params).toEqual({ msg: "5" });
  });

  it("parses /sessions/{id} with msg=last", () => {
    setURL("/sessions/abc-123?msg=last");
    const result = parsePath();
    expect(result.sessionId).toBe("abc-123");
    expect(result.params).toEqual({ msg: "last" });
  });

  it("parses page routes", () => {
    for (const route of [
      "usage",
      "trends",
      "insights",
      "pinned",
      "trash",
      "settings",
    ]) {
      setURL(`/${route}`);
      const result = parsePath();
      expect(result.route).toBe(route);
      expect(result.sessionId).toBeNull();
    }
  });

  it("parses /activity as a valid route", () => {
    window.history.replaceState({}, "", "/activity?preset=week&date=2026-06-16");
    const parsed = parsePath();
    expect(parsed.route).toBe("activity");
    expect(parsed.params.preset).toBe("week");
    expect(parsed.params.date).toBe("2026-06-16");
  });

  it("replaceParams writes query without a new history entry, keeping the path", () => {
    window.history.replaceState({}, "", "/activity");
    const store = new RouterStore();
    const before = window.history.length;
    store.replaceParams({ preset: "month", date: "2026-06-01" });
    expect(window.location.pathname).toBe("/activity");
    expect(window.location.search).toContain("preset=month");
    expect(window.location.search).toContain("date=2026-06-01");
    expect(window.history.length).toBe(before);
  });

  it("falls back to default for unknown routes", () => {
    setURL("/unknown");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.sessionId).toBeNull();
  });

  it("decodes encoded session IDs", () => {
    setURL("/sessions/copilot%3Aabc123");
    const result = parsePath();
    expect(result.sessionId).toBe("copilot:abc123");
  });

  it("falls back to raw segment on malformed percent encoding", () => {
    setURL("/sessions/foo%");
    const result = parsePath();
    expect(result.sessionId).toBe("foo%");
  });

  it("strips basePath from pathname", () => {
    const base = document.createElement("base");
    base.href = "/agentsview/";
    document.head.appendChild(base);
    try {
      setURL("/agentsview/sessions/abc");
      const result = parsePath();
      expect(result.route).toBe("sessions");
      expect(result.sessionId).toBe("abc");
    } finally {
      base.remove();
    }
  });
});

describe("RouterStore", () => {
  let store: RouterStore;

  afterEach(() => {
    store?.destroy();
    setURL("/");
  });

  it("initializes with parsed path", () => {
    setURL("/sessions?project=test");
    store = new RouterStore();
    expect(store.route).toBe("sessions");
    expect(store.params).toEqual({ project: "test" });
    expect(store.sessionId).toBeNull();
  });

  it("initializes sessionId from path", () => {
    setURL("/sessions/abc-123");
    store = new RouterStore();
    expect(store.route).toBe("sessions");
    expect(store.sessionId).toBe("abc-123");
  });

  it("falls back to default on invalid route", () => {
    setURL("/bogus");
    store = new RouterStore();
    expect(store.route).toBe("sessions");
  });

  it("navigate updates URL via pushState", () => {
    setURL("/");
    store = new RouterStore();
    const spy = vi.spyOn(window.history, "pushState");
    store.navigate("insights");
    expect(spy).toHaveBeenCalled();
    expect(store.route).toBe("insights");
    spy.mockRestore();
  });

  it("navigate updates URL to /trends", () => {
    setURL("/");
    store = new RouterStore();
    store.navigate("trends");
    expect(window.location.pathname).toBe("/trends");
    expect(store.route).toBe("trends");
  });

  it("navigate returns false on same URL (no-op)", () => {
    setURL("/sessions");
    store = new RouterStore();
    const result = store.navigate("sessions");
    expect(result).toBe(false);
  });

  it("navigate with params builds query string", () => {
    setURL("/");
    store = new RouterStore();
    store.navigate("sessions", { project: "foo" });
    expect(window.location.pathname).toBe("/sessions");
    expect(window.location.search).toBe("?project=foo");
  });

  it("navigateToSession updates URL to /sessions/{id}", () => {
    setURL("/sessions");
    store = new RouterStore();
    store.navigateToSession("abc-123");
    expect(window.location.pathname).toBe(
      "/sessions/abc-123",
    );
    expect(store.sessionId).toBe("abc-123");
  });

  it("navigateToSession with msg param", () => {
    setURL("/sessions");
    store = new RouterStore();
    store.navigateToSession("abc-123", { msg: "last" });
    expect(window.location.pathname).toBe(
      "/sessions/abc-123",
    );
    expect(window.location.search).toBe("?msg=last");
  });

  it("navigateToSession preserves session route params from the sessions view", () => {
    setURL(
      "/sessions?window_days=14&project=myproj&termination=unclean&msg=stale",
    );
    store = new RouterStore();
    store.navigateToSession("abc-123");

    expect(window.location.pathname).toBe(
      "/sessions/abc-123",
    );
    expect(window.location.search).toContain("window_days=14");
    expect(window.location.search).toContain("project=myproj");
    expect(window.location.search).toContain("termination=unclean");
    expect(window.location.search).not.toContain("msg=stale");
  });

  it("navigateToSession can clear stale preserved route params", () => {
    setURL(
      "/sessions?window_days=14&project=myproj&include_one_shot=false",
    );
    store = new RouterStore();
    store.navigateToSession("abc-123", undefined, ["include_one_shot"]);

    expect(window.location.pathname).toBe(
      "/sessions/abc-123",
    );
    expect(window.location.search).toContain("window_days=14");
    expect(window.location.search).toContain("project=myproj");
    expect(window.location.search).not.toContain("include_one_shot=false");
  });

  it("navigateToSessions preserves session route params for drilldowns", () => {
    setURL(
      "/sessions?date_from=2026-01-01&date_to=2026-01-31&project=myproj",
    );
    store = new RouterStore();
    store.navigateToSessions({ agent: "codex" });

    expect(window.location.pathname).toBe("/sessions");
    expect(window.location.search).toContain("date_from=2026-01-01");
    expect(window.location.search).toContain("date_to=2026-01-31");
    expect(window.location.search).toContain("project=myproj");
    expect(window.location.search).toContain("agent=codex");
  });

  it("navigateToSessions can clear preserved route params for drilldowns", () => {
    setURL(
      "/sessions?date_from=2026-01-01&date_to=2026-01-31&min_messages=10&max_messages=50",
    );
    store = new RouterStore();
    store.navigateToSessions(
      { min_messages: "100" },
      ["min_messages", "max_messages"],
    );

    expect(window.location.search).toContain("date_from=2026-01-01");
    expect(window.location.search).toContain("date_to=2026-01-31");
    expect(window.location.search).toContain("min_messages=100");
    expect(window.location.search).not.toContain("max_messages=50");
  });

  it("navigateToSession does not preserve params from non-session routes", () => {
    setURL("/usage?from=2026-01-01&to=2026-01-07");
    store = new RouterStore();
    store.navigateToSession("abc-123");

    expect(window.location.pathname).toBe(
      "/sessions/abc-123",
    );
    expect(window.location.search).toBe("");
  });

  it("navigateFromSession returns to /sessions", () => {
    setURL("/sessions/abc-123");
    store = new RouterStore();
    store.navigateFromSession();
    expect(window.location.pathname).toBe("/sessions");
    expect(store.sessionId).toBeNull();
  });

  it("navigateFromSession preserves filter params", () => {
    setURL("/sessions/abc-123");
    store = new RouterStore();
    store.navigateFromSession({ project: "myproj" });
    expect(window.location.pathname).toBe("/sessions");
    expect(window.location.search).toBe("?project=myproj");
  });

  it("responds to popstate events", () => {
    setURL("/sessions");
    store = new RouterStore();
    setURL("/insights");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(store.route).toBe("insights");
  });

  it("destroy removes popstate listener", () => {
    setURL("/");
    const addSpy = vi.spyOn(window, "addEventListener");
    store = new RouterStore();
    const registeredCb = addSpy.mock.calls.find(
      ([event]) => event === "popstate",
    )?.[1];
    addSpy.mockRestore();

    const removeSpy = vi.spyOn(
      window,
      "removeEventListener",
    );
    store.destroy();
    expect(removeSpy).toHaveBeenCalledWith(
      "popstate",
      registeredCb,
    );
    removeSpy.mockRestore();
  });

  it("replaceParams uses replaceState", () => {
    setURL("/sessions");
    store = new RouterStore();
    const spy = vi.spyOn(window.history, "replaceState");
    store.replaceParams({ project: "bar" });
    expect(spy).toHaveBeenCalled();
    expect(window.location.search).toBe("?project=bar");
    spy.mockRestore();
  });

  it("preserves desktop param across navigations", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    store.navigate("insights");
    expect(window.location.search).toBe("?desktop=");
    expect(store.params).toEqual({ desktop: "" });
  });

  it("preserves desktop param in navigateToSession", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    store.navigateToSession("abc-123");
    expect(window.location.pathname).toBe(
      "/sessions/abc-123",
    );
    expect(window.location.search).toBe("?desktop=");
    expect(store.params).toEqual({ desktop: "" });
  });

  it("preserves desktop param in navigateFromSession", () => {
    setURL("/sessions/abc-123?desktop");
    store = new RouterStore();
    store.navigateFromSession({ project: "myproj" });
    expect(window.location.search).toContain("desktop=");
    expect(window.location.search).toContain(
      "project=myproj",
    );
    expect(store.params).toEqual({
      desktop: "",
      project: "myproj",
    });
  });

  it("preserves desktop param in replaceParams", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    store.replaceParams({ project: "bar" });
    expect(window.location.search).toContain("desktop=");
    expect(window.location.search).toContain("project=bar");
    expect(store.params).toEqual({
      desktop: "",
      project: "bar",
    });
  });

  it("routing params override sticky params", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    store.navigate("sessions", { desktop: "off" });
    expect(window.location.search).toBe("?desktop=off");
  });

  it("updates sticky param value across navigations", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    store.navigate("sessions", { desktop: "off" });
    store.navigate("insights");
    expect(window.location.search).toBe("?desktop=off");
  });

  it("preserves sticky param across two consecutive navigations", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    store.navigate("insights");
    expect(window.location.search).toBe("?desktop=");
    store.navigate("pinned");
    expect(window.location.search).toBe("?desktop=");
  });

  it("refreshes sticky params on popstate", () => {
    setURL("/sessions?desktop=v1");
    store = new RouterStore();
    // Simulate browser back to a URL with different desktop value
    setURL("/insights?desktop=v2");
    window.dispatchEvent(new PopStateEvent("popstate"));
    // Next navigation should use updated sticky value
    store.navigate("pinned");
    expect(window.location.search).toBe("?desktop=v2");
  });

  it("removes sticky param on popstate to URL without it", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    setURL("/insights");
    window.dispatchEvent(new PopStateEvent("popstate"));
    store.navigate("pinned");
    expect(window.location.search).toBe("");
  });

  it("buildSessionHref includes sticky params", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    const href = store.buildSessionHref("abc-123");
    expect(href).toBe("/sessions/abc-123?desktop=");
  });

  it("buildSessionHref works without sticky params", () => {
    setURL("/sessions");
    store = new RouterStore();
    const href = store.buildSessionHref("abc-123");
    expect(href).toBe("/sessions/abc-123");
  });

  it("buildSessionHref preserves session route params from the sessions view", () => {
    setURL(
      "/sessions?window_days=14&project=myproj&termination=unclean&msg=stale",
    );
    store = new RouterStore();
    const href = store.buildSessionHref("abc-123");

    expect(href).toContain("/sessions/abc-123?");
    expect(href).toContain("window_days=14");
    expect(href).toContain("project=myproj");
    expect(href).toContain("termination=unclean");
    expect(href).not.toContain("msg=stale");
  });
});
