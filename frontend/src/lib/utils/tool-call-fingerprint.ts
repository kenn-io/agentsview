/**
 * Hashes a call's tool name and input with 32-bit FNV-1a over their UTF-8
 * bytes. It must match ToolCallRow.Fingerprint in internal/signals, which
 * the server sends with tool sequences and saves with report citations.
 */
export function toolCallFingerprint(toolName: string, inputJson: string): string {
  let hash = 0x811c9dc5;
  for (const byte of new TextEncoder().encode(`${toolName}\u0000${inputJson}`)) {
    hash = Math.imul(hash ^ byte, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, "0");
}
