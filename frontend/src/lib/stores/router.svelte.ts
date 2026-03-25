export type Route =
  | "sessions"
  | "insights"
  | "pinned"
  | "trash"
  | "settings";

const VALID_ROUTES: ReadonlySet<string> = new Set<Route>([
  "sessions",
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

/** Build a full URL path for a session, respecting basePath. */
export function buildSessionHref(id: string): string {
  return `${getBasePath()}/sessions/${encodeURIComponent(id)}`;
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

  const sessionId =
    route === "sessions" && segments.length >= 2
      ? decodeURIComponent(segments[1]!)
      : null;

  const params = Object.fromEntries(
    new URLSearchParams(window.location.search),
  );

  return { route, sessionId, params };
}

export class RouterStore {
  route: Route = $state("sessions");
  params: Record<string, string> = $state({});
  sessionId: string | null = $state(null);
  #onPopState: () => void;

  constructor() {
    const initial = parsePath();
    this.route = initial.route;
    this.params = initial.params;
    this.sessionId = initial.sessionId;

    this.#onPopState = () => {
      const parsed = parsePath();
      this.route = parsed.route;
      this.params = parsed.params;
      this.sessionId = parsed.sessionId;
    };
    window.addEventListener("popstate", this.#onPopState);
  }

  destroy() {
    window.removeEventListener(
      "popstate",
      this.#onPopState,
    );
  }

  #buildUrl(
    path: string,
    params: Record<string, string> = {},
  ): string {
    const basePath = getBasePath();
    const qs = new URLSearchParams(params).toString();
    const full = basePath + path;
    return qs ? `${full}?${qs}` : full;
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
    this.route = route;
    this.params = params;
    this.sessionId = null;
    window.history.pushState(null, "", url);
    return true;
  }

  navigateToSession(
    id: string,
    params: Record<string, string> = {},
  ) {
    const url = this.#buildUrl(
      `/sessions/${encodeURIComponent(id)}`,
      params,
    );
    this.route = "sessions";
    this.params = params;
    this.sessionId = id;
    window.history.pushState(null, "", url);
  }

  navigateFromSession(
    params: Record<string, string> = {},
  ) {
    const url = this.#buildUrl("/sessions", params);
    this.route = "sessions";
    this.params = params;
    this.sessionId = null;
    window.history.pushState(null, "", url);
  }

  /** Update query params without creating a history entry. */
  replaceParams(params: Record<string, string>) {
    const path = this.sessionId
      ? `/sessions/${encodeURIComponent(this.sessionId)}`
      : `/${this.route}`;
    const url = this.#buildUrl(path, params);
    this.params = params;
    window.history.replaceState(null, "", url);
  }
}

export const router = new RouterStore();
