interface SessionMachines {
  machine: string;
  source_machines?: string[];
}

/** Hosted copies can share one session while retaining several source devices. */
export function sessionMachineDisplay(
  session: SessionMachines,
  labels: Record<string, string>,
): { ids: string[]; label: string; title: string } {
  const sources = session.source_machines ?? [];
  const ids = sources.length ? sources : session.machine ? [session.machine] : [];
  return {
    ids,
    label: ids.map((id) => labels[id] ?? id).join(", "),
    title: ids
      .map((id) => (labels[id] && labels[id] !== id ? `${labels[id]} (${id})` : id))
      .join(", "),
  };
}
