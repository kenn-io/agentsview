/**
 * Hashes a call's tool name and input with 32-bit FNV-1a over their UTF-8
 * bytes. It must match toolCallFingerprint in internal/insight, which saves
 * the fingerprint a report's jump checks against.
 */
export function toolCallFingerprint(toolName: string, inputJson: string): string {
  let hash = 0x811c9dc5;
  for (const byte of new TextEncoder().encode(`${toolName}\u0000${inputJson}`)) {
    hash = Math.imul(hash ^ byte, 0x01000193) >>> 0;
  }
  return hash.toString(16).padStart(8, "0");
}
