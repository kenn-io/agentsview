import type { Route } from "@playwright/test";

interface MockSession {
  id: string;
  parent_session_id?: string | null;
  relationship_type?: string | null;
  project: string;
  machine: string;
  agent: string;
  first_message: string;
  display_name?: string | null;
  started_at: string;
  ended_at: string;
  message_count: number;
  user_message_count?: number;
  created_at: string;
  file_path: string;
  termination_status?: string | null;
  is_automated?: boolean;
  is_teammate?: boolean;
}

export function createMockSessions(
  count: number,
  prefix: string,
  projectFn: (index: number) => string,
): MockSession[] {
  const now = new Date().toISOString();
  return Array.from({ length: count }, (_, i) => ({
    id: `${prefix}-${i}`,
    project: projectFn(i),
    machine: "test-machine",
    agent: "test-agent",
    first_message: `Hello from ${prefix} ${i}`,
    started_at: now,
    ended_at: now,
    message_count: 10,
    user_message_count: 5,
    created_at: now,
    file_path: `/tmp/${prefix}-${i}.json`,
    is_automated: false,
    is_teammate: false,
  }));
}

interface SessionDataSet {
  sessions: MockSession[];
  project: string | null;
}

/**
 * Creates a route handler that serves paginated, filterable
 * session data from the provided data sets.
 */
export function handleSessionsRoute(dataSets: SessionDataSet[]) {
  return async (route: Route) => {
    const url = new URL(route.request().url());
    const pathname = url.pathname.replace(/\/+$/, "");
    const limit = Number(url.searchParams.get("limit") || "200");
    const cursor = url.searchParams.get("cursor");
    const project = url.searchParams.get("project");

    if (pathname.endsWith("/api/v1/sessions/sidebar-index")) {
      const filtered = filterSessions(dataSets, project);
      await route.fulfill({
        json: {
          sessions: filtered.map(toSidebarIndexRow),
          total: filtered.length,
        },
      });
      return;
    }

    const detailMatch = pathname.match(/\/api\/v1\/sessions\/([^/]+)$/);
    if (detailMatch) {
      const id = decodeURIComponent(detailMatch[1]!);
      const session = allSessions(dataSets).find((s) => s.id === id);
      if (!session) {
        await route.fulfill({
          status: 404,
          json: { error: "session not found" },
        });
        return;
      }
      await route.fulfill({ json: session });
      return;
    }

    if (!pathname.endsWith("/api/v1/sessions")) {
      await route.fallback();
      return;
    }

    const filtered = filterSessions(dataSets, project);
    const startIndex = cursor ? parseInt(cursor, 10) : 0;
    const slice = filtered.slice(startIndex, startIndex + limit);
    const nextCursor =
      startIndex + limit < filtered.length ? (startIndex + limit).toString() : undefined;

    await route.fulfill({
      json: {
        sessions: slice,
        next_cursor: nextCursor,
        total: filtered.length,
      },
    });
  };
}

export const sessionsRoutePattern = /\/api\/v1\/sessions(?:\/[^?]*)?(?:\?.*)?$/;

function filterSessions(dataSets: SessionDataSet[], project: string | null): MockSession[] {
  const defaultSet = dataSets.find((d) => d.project === null);
  const matchedSet = project ? dataSets.find((d) => d.project === project) : null;

  if (matchedSet) return matchedSet.sessions;
  if (!defaultSet) return [];
  return project ? defaultSet.sessions.filter((s) => s.project === project) : defaultSet.sessions;
}

function allSessions(dataSets: SessionDataSet[]): MockSession[] {
  const byID = new Map<string, MockSession>();
  for (const dataSet of dataSets) {
    for (const session of dataSet.sessions) {
      byID.set(session.id, session);
    }
  }
  return [...byID.values()];
}

function toSidebarIndexRow(session: MockSession) {
  return {
    id: session.id,
    parent_session_id: session.parent_session_id ?? null,
    relationship_type: session.relationship_type ?? null,
    project: session.project,
    machine: session.machine,
    agent: session.agent,
    display_name: session.display_name ?? null,
    started_at: session.started_at,
    ended_at: session.ended_at,
    created_at: session.created_at,
    termination_status: session.termination_status ?? null,
    message_count: session.message_count,
    user_message_count: session.user_message_count ?? session.message_count,
    is_automated: session.is_automated ?? false,
    is_teammate: session.is_teammate ?? false,
  };
}
