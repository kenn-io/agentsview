interface SessionParents {
  id: string;
  parent_session_id?: string | null;
  parent_session_ids?: string[];
}

/** Share indexed parent lookups and settled answers across one session pool. */
export function sessionAncestryMatcher<T extends SessionParents>(
  all: readonly T[],
  matches: (session: T) => boolean,
): (session: T) => boolean {
  let byId: Map<string, T> | undefined;
  const settled = new Map<string, boolean>();
  return (session) => {
    const pending: [T, string | undefined][] = [[session, undefined]];
    const visited = new Map<string, string | undefined>();
    while (pending.length) {
      const [current, from] = pending.pop()!;
      if (visited.has(current.id)) continue;
      const answer = settled.get(current.id);
      if (answer === false) continue;
      visited.set(current.id, from);
      if (answer === true || matches(current)) {
        // Only this path is known to reach the match; other branches may not.
        let id: string | undefined = current.id;
        while (id !== undefined) {
          settled.set(id, true);
          id = visited.get(id);
        }
        return true;
      }
      // Child-list reads supply the specific parent whose view is being shown.
      const parents = current.parent_session_id
        ? [current.parent_session_id]
        : (current.parent_session_ids ?? []);
      for (const id of parents) {
        if (!byId) {
          byId = new Map<string, T>();
          for (const session of all) {
            if (!byId.has(session.id)) byId.set(session.id, session);
          }
        }
        const parent = byId.get(id);
        if (parent) pending.push([parent, current.id]);
      }
    }
    for (const id of visited.keys()) settled.set(id, false);
    return false;
  };
}

/** Test the session and its loaded ancestors, honoring a contextual parent. */
export function sessionAncestryMatches<T extends SessionParents>(
  session: T,
  all: readonly T[],
  matches: (session: T) => boolean,
): boolean {
  return sessionAncestryMatcher(all, matches)(session);
}
