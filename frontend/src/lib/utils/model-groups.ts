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
): string {
  const tokens = parseCsv(csv);
  const remaining = tokens.filter(
    (token) => token !== name && !(Object.hasOwn(aliases, token) && aliases[token] === name),
  );
  return remaining.length === tokens.length ? [...tokens, name].join(",") : remaining.join(",");
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
