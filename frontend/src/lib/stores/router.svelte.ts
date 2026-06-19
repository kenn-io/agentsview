export type Route =
  | "sessions"
  | "usage"
  | "activity"
  | "trends"
  | "insights"
  | "pinned"
  | "trash"
  | "settings";

const VALID_ROUTES: ReadonlySet<string> = new Set<Route>([
  "sessions",
  "usage",
  "activity",
  "trends",
  "insights",
  "pinned",
  "trash",
  "settings",
]);

const DEFAULT_ROUTE: Route = "sessions";

export function getBasePath(): string {
  const base = document.querySelector("base");
  if (!base) return "";
  const href = base.getAttribute("href") ?? "";
  return href.replace(/\/+$/, "");
}


export function parsePath(): {
  route: Route;
  sessionId: string | null;
  params: Record<string, string>;
} {
  const basePath = getBasePath();
  let pathname = window.location.pathname;
  if (basePath && pathname.startsWith(basePath)) {
    pathname = pathname.slice(basePath.length);
  }
  if (!pathname.startsWith("/")) pathname = "/" + pathname;

  const segments = pathname
    .split("/")
    .filter((s) => s.length > 0);
  const routeStr = segments[0] ?? "";
  const route: Route = VALID_ROUTES.has(routeStr)
    ? (routeStr as Route)
    : DEFAULT_ROUTE;

  let sessionId: string | null = null;
  if (route === "sessions" && segments.length >= 2) {
    try {
      sessionId = decodeURIComponent(segments[1]!);
    } catch {
      sessionId = segments[1]!;
    }
  }

  const params = Object.fromEntries(
    new URLSearchParams(window.location.search),
  );

  return { route, sessionId, params };
}

/** Params that are not part of routing but must survive navigations. */
const STICKY_PARAMS = new Set(["desktop"]);
const SESSION_ROUTE_PARAMS = new Set([
  "project",
  "machine",
  "agent",
  "termination",
  "date",
  "date_from",
  "date_to",
  "active_since",
  "exclude_project",
  "min_messages",
  "max_messages",
  "min_user_messages",
  "include_one_shot",
  "include_automated",
  "window_days",
]);

export class RouterStore {
  route: Route = $state("sessions");
  params: Record<string, string> = $state({});
  sessionId: string | null = $state(null);
  #onPopState: () => void;
  #stickyParams: Record<string, string>;

  constructor() {
    const initial = parsePath();
    this.route = initial.route;
    this.params = initial.params;
    this.sessionId = initial.sessionId;

    this.#stickyParams = {};
    for (const [k, v] of Object.entries(initial.params)) {
      if (STICKY_PARAMS.has(k)) {
        this.#stickyParams[k] = v;
      }
    }

    this.#onPopState = () => {
      const parsed = parsePath();
      this.route = parsed.route;
      this.params = parsed.params;
      this.sessionId = parsed.sessionId;
      this.#replaceSticky(parsed.params);
    };
    window.addEventListener("popstate", this.#onPopState);
  }

  destroy() {
    window.removeEventListener(
      "popstate",
      this.#onPopState,
    );
  }

  /** Update sticky params that are explicitly present in params. */
  #updateSticky(params: Record<string, string>) {
    for (const key of STICKY_PARAMS) {
      if (key in params) {
        this.#stickyParams[key] = params[key]!;
      }
    }
  }

  /** Full replace of sticky state from complete URL params (popstate). */
  #replaceSticky(params: Record<string, string>) {
    for (const key of STICKY_PARAMS) {
      if (key in params) {
        this.#stickyParams[key] = params[key]!;
      } else {
        delete this.#stickyParams[key];
      }
    }
  }

  #buildUrl(
    path: string,
    params: Record<string, string> = {},
  ): string {
    const basePath = getBasePath();
    const merged = { ...this.#stickyParams, ...params };
    const qs = new URLSearchParams(merged).toString();
    const full = basePath + path;
    return qs ? `${full}?${qs}` : full;
  }

  #sessionRouteParams(): Record<string, string> {
    if (this.route !== "sessions") return {};
    const params: Record<string, string> = {};
    for (const [key, value] of Object.entries(this.params)) {
      if (SESSION_ROUTE_PARAMS.has(key)) {
        params[key] = value;
      }
    }
    return params;
  }

  #sessionEntryParams(
    params: Record<string, string> | undefined,
    clearParams: Iterable<string> = [],
  ): Record<string, string> {
    const current = this.#sessionRouteParams();
    for (const key of clearParams) {
      delete current[key];
    }
    return {
      ...current,
      ...(params ?? {}),
    };
  }

  /** Build an href for a session link (includes sticky params). */
  buildSessionHref(
    id: string,
    params?: Record<string, string>,
  ): string {
    return this.#buildUrl(
      `/sessions/${encodeURIComponent(id)}`,
      this.#sessionEntryParams(params),
    );
  }

  navigate(
    route: Route,
    params: Record<string, string> = {},
  ): boolean {
    const url = this.#buildUrl(`/${route}`, params);
    if (
      url ===
      window.location.pathname + window.location.search
    ) {
      return false;
    }
    this.#updateSticky(params);
    this.route = route;
    this.params = { ...this.#stickyParams, ...params };
    this.sessionId = null;
    window.history.pushState(null, "", url);
    return true;
  }

  navigateToSessions(
    params: Record<string, string> = {},
    clearParams: Iterable<string> = [],
  ): boolean {
    return this.navigate(
      "sessions",
      this.#sessionEntryParams(params, clearParams),
    );
  }

  navigateToSession(
    id: string,
    params?: Record<string, string>,
    clearParams: Iterable<string> = [],
  ) {
    const nextParams = this.#sessionEntryParams(params, clearParams);
    const url = this.#buildUrl(
      `/sessions/${encodeURIComponent(id)}`,
      nextParams,
    );
    this.#updateSticky(nextParams);
    this.route = "sessions";
    this.params = { ...this.#stickyParams, ...nextParams };
    this.sessionId = id;
    window.history.pushState(null, "", url);
  }

  navigateFromSession(
    params: Record<string, string> = {},
  ) {
    const url = this.#buildUrl("/sessions", params);
    this.#updateSticky(params);
    this.route = "sessions";
    this.params = { ...this.#stickyParams, ...params };
    this.sessionId = null;
    window.history.pushState(null, "", url);
  }

  /** Update query params without creating a history entry. */
  replaceParams(params: Record<string, string>) {
    const path = this.sessionId
      ? `/sessions/${encodeURIComponent(this.sessionId)}`
      : `/${this.route}`;
    const url = this.#buildUrl(path, params);
    this.#updateSticky(params);
    this.params = { ...this.#stickyParams, ...params };
    window.history.replaceState(null, "", url);
  }
}

export const router = new RouterStore();
