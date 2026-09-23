interface SessionParents {
  id: string;
  parent_session_id?: string | null;
  parent_session_ids?: string[];
}

/** Test the session and its loaded ancestors, honoring a contextual parent. */
export function sessionAncestryMatches<T extends SessionParents>(
  session: T,
  all: readonly T[],
  matches: (session: T) => boolean,
): boolean {
  const pending = [session];
  const visited = new Set<string>();
  while (pending.length) {
    const current = pending.pop()!;
    if (visited.has(current.id)) continue;
    visited.add(current.id);
    if (matches(current)) return true;
    // Child-list reads supply the specific parent whose view is being shown.
    const parents = current.parent_session_id
      ? [current.parent_session_id]
      : (current.parent_session_ids ?? []);
    for (const id of parents) {
      const parent = all.find((candidate) => candidate.id === id);
      if (parent) pending.push(parent);
    }
  }
  return false;
}
