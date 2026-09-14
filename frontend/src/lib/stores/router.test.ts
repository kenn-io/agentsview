import { describe, it, expect, vi, afterEach } from "vite-plus/test";
import { parsePath, RouterStore } from "./router.svelte.js";

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
    expect(result.isRootPath).toBe(true);
  });

  it("marks root independently of query parameters", () => {
    setURL("/?desktop=1");
    expect(parsePath().isRootPath).toBe(true);

    setURL("/sessions?desktop=1");
    expect(parsePath().isRootPath).toBe(false);
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
    for (const route of ["usage", "trends", "recall", "quality", "pinned", "trash", "settings"]) {
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

  it("falls back from the removed insights route while preserving session params", () => {
    setURL("/insights?window_days=30&date_from=2026-07-01");
    const result = parsePath();
    expect(result.route).toBe("sessions");
    expect(result.params).toEqual({
      window_days: "30",
      date_from: "2026-07-01",
    });
  });

  it("decodes encoded session IDs", () => {
    setURL("/sessions/copilot%3Aabc123");
    const result = parsePath();
    expect(result.sessionId).toBe("copilot:abc123");
    expect(window.location.pathname).toBe("/sessions/copilot%3Aabc123");
  });

  it("parses the issue provider/raw-ID URL into the canonical ID", () => {
    setURL("/sessions/codex/01a0a016-fb25-72b2-a65f-30570dbb97aa");
    expect(parsePath().sessionId).toBe("codex:01a0a016-fb25-72b2-a65f-30570dbb97aa");
  });

  it.each([
    ["/sessions/provider/part%3Adetail", "provider:part:detail"],
    ["/sessions/pr%2Fovider/raw%2Fid%252F%3F%23", "pr/ovider:raw/id%2F?#"],
    ["/sessions/copilot%3Araw%252Fid", "copilot:raw%2Fid"],
    ["/sessions/host~abc-123", "host~abc-123"],
    ["/sessions", null],
    ["/usage/codex/abc", null],
  ])("preserves mode/state identity for %s", (path, id) => {
    setURL(path);
    expect(parsePath().sessionId).toBe(id);
    expect(window.location.pathname).toBe(path);
  });

  it.each([
    ["/sessions/prov%/raw%20id", "prov%:raw id"],
    ["/sessions/prov%20ider/raw%E0%A4", "prov ider:raw%E0%A4"],
    ["/sessions/prov%/raw%", "prov%:raw%"],
    ["/sessions/copilot%3Araw%", "copilot%3Araw%"],
  ])("falls back independently on malformed percent encoding in %s", (path, id) => {
    setURL(path);
    expect(parsePath().sessionId).toBe(id);
    expect(window.location.pathname).toBe(path);
  });

  it.each([
    ["/sessions/codex", "codex"],
    ["/sessions/codex/abc", "codex:abc"],
    ["/sessions/codex/abc/ignored", "codex:abc"],
  ])("consumes at most 2 session segments in %s", (path, id) => {
    setURL(path);
    expect(parsePath().sessionId).toBe(id);
    expect(window.location.pathname).toBe(path);
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
      expect(result.isRootPath).toBe(false);

      setURL("/agentsview/?desktop=1");
      expect(parsePath().isRootPath).toBe(true);
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

  it("reproduces the issue URL through buildSessionHref and parsing", () => {
    setURL("/sessions");
    store = new RouterStore();
    setURL(store.buildSessionHref("codex:01a0a016-fb25-72b2-a65f-30570dbb97aa"));
    expect(window.location.pathname).toBe("/sessions/codex/01a0a016-fb25-72b2-a65f-30570dbb97aa");
    expect(parsePath().sessionId).toBe("codex:01a0a016-fb25-72b2-a65f-30570dbb97aa");
  });

  it.each([
    ["codex:abc-123", "/sessions/codex/abc-123"],
    ["provider:part:detail", "/sessions/provider/part%3Adetail"],
    ["pr/ovider:raw/id%2F?#", "/sessions/pr%2Fovider/raw%2Fid%252F%3F%23"],
    ["提供者:雪 id", "/sessions/%E6%8F%90%E4%BE%9B%E8%80%85/%E9%9B%AA%20id"],
    ["host~codex:abc", "/sessions/host~codex/abc"],
    ["abc-123", "/sessions/abc-123"],
    ["host~raw/id%?#", "/sessions/host~raw%2Fid%25%3F%23"],
  ])("buildSessionHref and navigateToSession round-trip %s", (id, path) => {
    setURL("/sessions");
    store = new RouterStore();
    const href = store.buildSessionHref(id);
    expect(href).toBe(path);
    setURL(href);
    expect(window.location.pathname).toBe(path);
    expect(parsePath().sessionId).toBe(id);

    store.navigateToSession(id, { msg: "last" });
    expect(window.location.pathname).toBe(path);
    expect(window.location.search).toBe("?msg=last");
    expect(store.sessionId).toBe(id);
    expect(parsePath()).toEqual({
      route: "sessions",
      sessionId: id,
      params: { msg: "last" },
      isRootPath: false,
    });
  });

  it.each(["", "/agentsview"])(
    "preserves filters, sticky params and history under base path '%s'",
    (basePath) => {
      const base = document.createElement("base");
      base.href = `${basePath}/`;
      document.head.appendChild(base);
      try {
        setURL(`${basePath}/sessions?desktop=on&project=old&window_days=14&starred=true&msg=stale`);
        store = new RouterStore();
        const path = `${basePath}/sessions/codex/abc`;
        const params = {
          desktop: "off",
          project: "new",
          window_days: "14",
          starred: "true",
          msg: "last",
        };
        const href = store.buildSessionHref("codex:abc", {
          desktop: "off",
          project: "new",
          msg: "last",
        });
        setURL(href);
        expect(window.location.pathname).toBe(path);
        expect(parsePath()).toEqual({
          route: "sessions",
          sessionId: "codex:abc",
          params,
          isRootPath: false,
        });
        expect(store.params.desktop).toBe("on");

        const before = window.history.length;
        store.navigateToSession("codex:abc", { desktop: "off", project: "new", msg: "last" }, [
          "starred",
        ]);
        expect(window.history.length).toBe(before + 1);
        expect(window.location.pathname).toBe(path);
        expect(window.location.search).toBe("?desktop=off&project=new&window_days=14&msg=last");
        expect(store.params).toEqual({
          desktop: "off",
          project: "new",
          window_days: "14",
          msg: "last",
        });
        expect(store.sessionId).toBe("codex:abc");
        expect(store.route).toBe("sessions");
        expect(store.isRootPath).toBe(false);
        expect(parsePath().sessionId).toBe("codex:abc");

        store.replaceParams({ project: "replacement", msg: "2" });
        expect(window.history.length).toBe(before + 1);
        expect(window.location.pathname).toBe(path);
        expect(window.location.search).toBe("?desktop=off&project=replacement&msg=2");
        expect(store.params).toEqual({ desktop: "off", project: "replacement", msg: "2" });
        expect(store.sessionId).toBe("codex:abc");
        expect(parsePath().sessionId).toBe("codex:abc");

        setURL(`${basePath}/sessions/provider/part%3Adetail?desktop=back&project=history&msg=3`);
        window.dispatchEvent(new PopStateEvent("popstate"));
        expect(window.location.pathname).toBe(`${basePath}/sessions/provider/part%3Adetail`);
        expect(store.sessionId).toBe("provider:part:detail");
        expect(store.route).toBe("sessions");
        expect(store.params).toEqual({ desktop: "back", project: "history", msg: "3" });
        expect(store.isRootPath).toBe(false);
        expect(store.buildSessionHref("codex:next")).toBe(
          `${basePath}/sessions/codex/next?desktop=back&project=history`,
        );

        store.navigateFromSession({ project: "history" });
        expect(window.location.pathname).toBe(`${basePath}/sessions`);
        expect(window.location.search).toBe("?desktop=back&project=history");
        expect(store.sessionId).toBeNull();
      } finally {
        base.remove();
      }
    },
  );

  it("replaceParams preserves a directly loaded provider/raw-ID path", () => {
    setURL("/sessions/provider/part%3Adetail?desktop=on&project=old");
    store = new RouterStore();
    const before = window.history.length;
    store.replaceParams({ project: "new", desktop: "off" });
    expect(window.location.pathname).toBe("/sessions/provider/part%3Adetail");
    expect(window.location.search).toBe("?desktop=off&project=new");
    expect(window.history.length).toBe(before);
    expect(store.sessionId).toBe("provider:part:detail");
    expect(parsePath().sessionId).toBe("provider:part:detail");
  });

  it.each(["/sessions/copilot%3Aabc123", "/sessions/copilot/abc123"])(
    "hands the canonical ID to consumers from %s and popstate",
    (path) => {
      setURL(path);
      store = new RouterStore();
      expect(store.sessionId).toBe("copilot:abc123");
      expect(window.location.pathname).toBe(path);
      store.navigate("usage");
      expect(store.sessionId).toBeNull();
      setURL(`${path}?msg=5`);
      window.dispatchEvent(new PopStateEvent("popstate"));
      expect(store.sessionId).toBe("copilot:abc123");
      expect(store.params).toEqual({ msg: "5" });
      expect(store.route).toBe("sessions");
      expect(store.isRootPath).toBe(false);
      expect(window.location.pathname).toBe(path);
    },
  );

  it("initializes with parsed path", () => {
    setURL("/sessions?project=test");
    store = new RouterStore();
    expect(store.route).toBe("sessions");
    expect(store.params).toEqual({ project: "test" });
    expect(store.sessionId).toBeNull();
    expect(store.isRootPath).toBe(false);
  });

  it("initializes root state from the current pathname", () => {
    setURL("/?desktop=1");
    store = new RouterStore();
    expect(store.isRootPath).toBe(true);
  });

  it("updates root state on popstate", () => {
    setURL("/sessions");
    store = new RouterStore();
    setURL("/?desktop=1");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(store.isRootPath).toBe(true);
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
    store.navigate("quality");
    expect(spy).toHaveBeenCalled();
    expect(store.route).toBe("quality");
    expect(store.isRootPath).toBe(false);
    spy.mockRestore();
  });

  it.each([
    ["navigate", (router: RouterStore) => router.navigate("quality")],
    ["replace", (router: RouterStore) => router.replace("quality")],
    ["navigateToSession", (router: RouterStore) => router.navigateToSession("abc-123")],
    ["navigateFromSession", (router: RouterStore) => router.navigateFromSession()],
    ["replaceParams", (router: RouterStore) => router.replaceParams({ project: "test" })],
  ] as const)("clears root state through %s", (_name, navigate) => {
    setURL("/");
    store = new RouterStore();
    expect(store.isRootPath).toBe(true);

    navigate(store);

    expect(store.isRootPath).toBe(false);
  });

  it("replaces a route and preserves supplied params without adding history", () => {
    setURL("/token-usage?project=demo&desktop");
    store = new RouterStore();
    const before = window.history.length;
    const replaceSpy = vi.spyOn(window.history, "replaceState");

    store.replace("usage", {
      project: "demo",
      desktop: "",
      view: "tokens",
    });

    expect(store.route).toBe("usage");
    expect(store.params).toEqual({
      project: "demo",
      desktop: "",
      view: "tokens",
    });
    expect(window.location.pathname).toBe("/usage");
    expect(window.location.search).toContain("view=tokens");
    expect(window.history.length).toBe(before);
    expect(replaceSpy).toHaveBeenCalledOnce();
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
    expect(window.location.pathname).toBe("/sessions/abc-123");
    expect(store.sessionId).toBe("abc-123");
  });

  it("navigateToSession with msg param", () => {
    setURL("/sessions");
    store = new RouterStore();
    store.navigateToSession("abc-123", { msg: "last" });
    expect(window.location.pathname).toBe("/sessions/abc-123");
    expect(window.location.search).toBe("?msg=last");
  });

  it("navigateToSession preserves session route params from the sessions view", () => {
    setURL("/sessions?window_days=14&project=myproj&termination=unclean&msg=stale");
    store = new RouterStore();
    store.navigateToSession("abc-123");

    expect(window.location.pathname).toBe("/sessions/abc-123");
    expect(window.location.search).toContain("window_days=14");
    expect(window.location.search).toContain("project=myproj");
    expect(window.location.search).toContain("termination=unclean");
    expect(window.location.search).not.toContain("msg=stale");
  });

  it("navigateToSession can clear stale preserved route params", () => {
    setURL("/sessions?window_days=14&project=myproj&include_one_shot=false");
    store = new RouterStore();
    store.navigateToSession("abc-123", undefined, ["include_one_shot"]);

    expect(window.location.pathname).toBe("/sessions/abc-123");
    expect(window.location.search).toContain("window_days=14");
    expect(window.location.search).toContain("project=myproj");
    expect(window.location.search).not.toContain("include_one_shot=false");
  });

  it("navigateToSessions preserves session route params for drilldowns", () => {
    setURL("/sessions?date_from=2026-01-01&date_to=2026-01-31&project=myproj");
    store = new RouterStore();
    store.navigateToSessions({ agent: "codex" });

    expect(window.location.pathname).toBe("/sessions");
    expect(window.location.search).toContain("date_from=2026-01-01");
    expect(window.location.search).toContain("date_to=2026-01-31");
    expect(window.location.search).toContain("project=myproj");
    expect(window.location.search).toContain("agent=codex");
  });

  it("navigateToSessions can clear preserved route params for drilldowns", () => {
    setURL("/sessions?date_from=2026-01-01&date_to=2026-01-31&min_messages=10&max_messages=50");
    store = new RouterStore();
    store.navigateToSessions({ min_messages: "100" }, ["min_messages", "max_messages"]);

    expect(window.location.search).toContain("date_from=2026-01-01");
    expect(window.location.search).toContain("date_to=2026-01-31");
    expect(window.location.search).toContain("min_messages=100");
    expect(window.location.search).not.toContain("max_messages=50");
  });

  it("navigateToSession does not preserve params from non-session routes", () => {
    setURL("/usage?from=2026-01-01&to=2026-01-07");
    store = new RouterStore();
    store.navigateToSession("abc-123");

    expect(window.location.pathname).toBe("/sessions/abc-123");
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
    setURL("/quality");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(store.route).toBe("quality");
  });

  it("destroy removes popstate listener", () => {
    setURL("/");
    const addSpy = vi.spyOn(window, "addEventListener");
    store = new RouterStore();
    const registeredCb = addSpy.mock.calls.find(([event]) => event === "popstate")?.[1];
    addSpy.mockRestore();

    const removeSpy = vi.spyOn(window, "removeEventListener");
    store.destroy();
    expect(removeSpy).toHaveBeenCalledWith("popstate", registeredCb);
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
    store.navigate("quality");
    expect(window.location.search).toBe("?desktop=");
    expect(store.params).toEqual({ desktop: "" });
  });

  it("preserves desktop param in navigateToSession", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    store.navigateToSession("abc-123");
    expect(window.location.pathname).toBe("/sessions/abc-123");
    expect(window.location.search).toBe("?desktop=");
    expect(store.params).toEqual({ desktop: "" });
  });

  it("preserves desktop param in navigateFromSession", () => {
    setURL("/sessions/abc-123?desktop");
    store = new RouterStore();
    store.navigateFromSession({ project: "myproj" });
    expect(window.location.search).toContain("desktop=");
    expect(window.location.search).toContain("project=myproj");
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
    store.navigate("quality");
    expect(window.location.search).toBe("?desktop=off");
  });

  it("preserves sticky param across two consecutive navigations", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    store.navigate("quality");
    expect(window.location.search).toBe("?desktop=");
    store.navigate("pinned");
    expect(window.location.search).toBe("?desktop=");
  });

  it("refreshes sticky params on popstate", () => {
    setURL("/sessions?desktop=v1");
    store = new RouterStore();
    // Simulate browser back to a URL with different desktop value
    setURL("/quality?desktop=v2");
    window.dispatchEvent(new PopStateEvent("popstate"));
    // Next navigation should use updated sticky value
    store.navigate("pinned");
    expect(window.location.search).toBe("?desktop=v2");
  });

  it("removes sticky param on popstate to URL without it", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    setURL("/quality");
    window.dispatchEvent(new PopStateEvent("popstate"));
    store.navigate("pinned");
    expect(window.location.search).toBe("");
  });

  it("buildHref includes sticky params when active", () => {
    setURL("/sessions?desktop");
    store = new RouterStore();
    const href = store.buildHref("data", { project_key: "pl1:sha256:alpha" });
    expect(href).toBe("/data?desktop=&project_key=pl1%3Asha256%3Aalpha");
  });

  it("buildHref omits sticky params when inactive", () => {
    setURL("/sessions");
    store = new RouterStore();
    const href = store.buildHref("data", { project_key: "k1" });
    expect(href).toBe("/data?project_key=k1");
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
    setURL("/sessions?window_days=14&project=myproj&termination=unclean&msg=stale");
    store = new RouterStore();
    const href = store.buildSessionHref("abc-123");

    expect(href).toContain("/sessions/abc-123?");
    expect(href).toContain("window_days=14");
    expect(href).toContain("project=myproj");
    expect(href).toContain("termination=unclean");
    expect(href).not.toContain("msg=stale");
  });

  it("buildSessionHref preserves starred-only filtering", () => {
    setURL("/sessions?starred=true");
    store = new RouterStore();

    const href = store.buildSessionHref("abc-123");

    expect(href).toBe("/sessions/abc-123?starred=true");
  });
});
