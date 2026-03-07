type Route = "sessions" | "insights" | "pinned" | "trash";

const VALID_ROUTES: ReadonlySet<string> = new Set<Route>([
  "sessions",
  "insights",
  "pinned",
  "trash",
]);

const DEFAULT_ROUTE: Route = "sessions";

export function parseHash(): {
  route: Route;
  params: Record<string, string>;
} {
  const hash = window.location.hash.slice(1);
  if (!hash || hash === "/") {
    return { route: DEFAULT_ROUTE, params: {} };
  }

  const qIdx = hash.indexOf("?");
  const path = qIdx >= 0 ? hash.slice(0, qIdx) : hash;
  const routeString = path.startsWith("/")
    ? path.slice(1)
    : path;
  const route: Route = VALID_ROUTES.has(routeString)
    ? (routeString as Route)
    : DEFAULT_ROUTE;

  const params =
    qIdx >= 0
      ? Object.fromEntries(
          new URLSearchParams(hash.slice(qIdx + 1)),
        )
      : {};

  return { route, params };
}

export class RouterStore {
  route: Route = $state("sessions");
  params: Record<string, string> = $state({});
  #onHashChange: () => void;
  #lastHash: string = "";

  constructor() {
    const initial = parseHash();
    this.route = initial.route;
    this.params = initial.params;

    this.#onHashChange = () => {
      const currentHash = window.location.hash;
      if (currentHash === this.#lastHash) {
        this.#lastHash = "";
        return;
      }
      this.#lastHash = "";
      const parsed = parseHash();
      this.route = parsed.route;
      this.params = parsed.params;
    };
    window.addEventListener("hashchange", this.#onHashChange);
  }

  destroy() {
    window.removeEventListener(
      "hashchange",
      this.#onHashChange,
    );
  }

  navigate(route: Route, params: Record<string, string> = {}) {
    const qs = new URLSearchParams(params).toString();
    const hash = qs ? `#/${route}?${qs}` : `#/${route}`;
    if (hash === window.location.hash) return;
    this.route = route;
    this.params = params;
    this.#lastHash = hash;
    window.location.hash = hash;
  }
}

export const router = new RouterStore();
