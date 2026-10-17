export function groupedModelCsv(csv: string, aliases: Record<string, string>): string {
  const names = csv
    .split(",")
    .filter(Boolean)
    .map((name) => (Object.hasOwn(aliases, name) ? aliases[name] : name));
  return [...new Set(names)].join(",");
}

export function toggleGroupedModel(
  csv: string,
  name: string,
  aliases: Record<string, string>,
): string {
  const tokens = csv.split(",").filter(Boolean);
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
  const selected = selectedCsv.split(",").filter(Boolean);
  return groupedModelCsv([...known, selectedCsv].join(","), aliases)
    .split(",")
    .filter(Boolean)
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
