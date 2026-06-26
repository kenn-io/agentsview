export function localDateStr(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

export function daysAgo(n: number): string {
  const d = new Date();
  d.setDate(d.getDate() - n);
  return localDateStr(d);
}

export function today(): string {
  return localDateStr(new Date());
}

export function rollingRange(
  days: number,
  now: Date = new Date(),
): { from: string; to: string } {
  const to = new Date(now);
  const from = new Date(to);
  from.setDate(from.getDate() - days);
  return {
    from: localDateStr(from),
    to: localDateStr(to),
  };
}

export function parseLocalDate(value: string): Date | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) return null;
  const d = new Date(value + "T00:00:00");
  return Number.isNaN(d.getTime()) ? null : d;
}

export function addDays(value: string, days: number): string {
  const d = parseLocalDate(value);
  if (!d) return "";
  d.setDate(d.getDate() + days);
  return localDateStr(d);
}

export function startOfIsoWeek(value: string): string {
  const d = parseLocalDate(value);
  if (!d) return "";
  const offset = (d.getDay() + 6) % 7;
  d.setDate(d.getDate() - offset);
  return localDateStr(d);
}

export function startOfMonth(value: string): string {
  const d = parseLocalDate(value);
  if (!d) return "";
  return localDateStr(new Date(d.getFullYear(), d.getMonth(), 1));
}

export function endOfMonth(value: string): string {
  const d = parseLocalDate(value);
  if (!d) return "";
  return localDateStr(new Date(d.getFullYear(), d.getMonth() + 1, 0));
}
