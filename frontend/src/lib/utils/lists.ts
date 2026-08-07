// ABOUTME: Shared helper for separator-joined list state (CSV filters and
// ABOUTME: branch-token lists) used by the sessions and usage stores.

/**
 * Toggle a value's membership in a separator-joined list string: present
 * values are removed, absent values appended. The separator must be a
 * character that cannot appear inside values (comma for plain names, the
 * branch list separator for branch tokens).
 */
export function toggleListValue(
  list: string,
  value: string,
  sep: string,
): string {
  const current = list ? list.split(sep) : [];
  const idx = current.indexOf(value);
  if (idx >= 0) {
    current.splice(idx, 1);
  } else {
    current.push(value);
  }
  return current.join(sep);
}
