import {
  describe,
  it,
  expect,
  vi,
  afterEach,
} from "vitest";
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
});
