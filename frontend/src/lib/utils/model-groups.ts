function parseCsv(csv: string): string[] {
  return csv
    .split(",")
    .map((token) => token.trim())
    .filter(Boolean);
}

export function groupedModelCsv(csv: string, aliases: Record<string, string>): string {
  const names = parseCsv(csv).map((name) => (Object.hasOwn(aliases, name) ? aliases[name] : name));
  return [...new Set(names)].join(",");
}

export function toggleGroupedModel(
  csv: string,
  name: string,
  aliases: Record<string, string>,
  exclude = false,
): string {
  const tokens = parseCsv(csv);
  const kept = tokens.filter((t) => ![t, Object.hasOwn(aliases, t) && aliases[t]].includes(name));
  const add = exclude ? !tokens.includes(name) : kept.length === tokens.length;
  return (add ? [...kept, name] : kept).join(",");
}

export function modelPickerItems(
  known: string[],
  selectedCsv: string,
  aliases: Record<string, string>,
) {
  const selected = parseCsv(selectedCsv);
  return parseCsv(groupedModelCsv([...known, selectedCsv].join(","), aliases))
    .sort()
    .map((name) => ({
      name,
      description: selected.includes(name)
        ? undefined
        : selected
            .filter((token) => Object.hasOwn(aliases, token) && aliases[token] === name)
            .join(", ") || undefined,
    }));
}

// Exclude mode shows a group unticked only when its name is excluded;
// individually hidden members count as partial so the group stays ticked.
export function excludedModelGroups(csv: string, aliases: Record<string, string>) {
  const tokens = parseCsv(csv);
  const group = (name: string) => (Object.hasOwn(aliases, name) ? aliases[name]! : name);
  const full = parseCsv(groupedModelCsv(csv, aliases)).filter((name) => tokens.includes(name));
  return {
    csv: full.join(","),
    partial: tokens.filter((token) => !full.includes(group(token))).length,
  };
}
