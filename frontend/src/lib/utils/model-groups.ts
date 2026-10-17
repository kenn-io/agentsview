export function groupedModelCsv(csv: string, aliases: Record<string, string>): string {
  const names = csv.split(",").filter(Boolean).map((name) => aliases[name] ?? name);
  return [...new Set(names)].join(",");
}

export function toggleGroupedModel(
  csv: string,
  name: string,
  aliases: Record<string, string>,
): string {
  const tokens = csv.split(",").filter(Boolean);
  const remaining = tokens.filter((token) => token !== name && aliases[token] !== name);
  return remaining.length === tokens.length ? [...tokens, name].join(",") : remaining.join(",");
}
