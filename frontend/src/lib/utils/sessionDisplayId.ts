/** The session ID as people read it: without the `host:` prefix that a
 *  session from another archive carries. */
export function sessionDisplayId(id: string): string {
  const idx = id.indexOf(":");
  return idx >= 0 ? id.slice(idx + 1) : id;
}
