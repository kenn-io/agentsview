import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex, hexToBytes } from "@noble/hashes/utils.js";
import type { DbMessage } from "../api/generated/index.js";

function fingerprint(message: DbMessage): string {
  const data = JSON.stringify([message.role, message.timestamp, message.content, message.thinking_text, message.tool_calls ?? []]);
  return bytesToHex(sha256(new TextEncoder().encode(data)));
}

export function messageIdentity(message: DbMessage): string {
  return `${fingerprint(message)}.${bytesToHex(new TextEncoder().encode(message.source_uuid ?? ""))}.${message.id}`;
}

export function messageIdentityHasSource(identity: string): boolean {
  return /^[0-9a-f]{64}\.[0-9a-f]+\.\d+$/.test(identity);
}

/** Check the copied row ID against its fingerprint before using the fast path. */
export function exactMessageIdentity(messages: readonly DbMessage[], identity: string, ordinal: number): number | null {
  const parts = /^([0-9a-f]{64})\.([0-9a-f]*)\.(\d+)$/.exec(identity);
  if (!parts) return null;
  const row = messages.find((message) => String(message.id) === parts[3]);
  return row && row.ordinal === ordinal && messageIdentity(row).split(".")[1] === parts[2] && fingerprint(row) === parts[1] ? row.ordinal : null;
}

/** Resolve only an identity that names exactly one message in a complete transcript. */
export function resolveMessageIdentity(messages: readonly DbMessage[], identity: string): number | null {
  const parts = /^([0-9a-f]{64})\.([0-9a-f]*)\.(\d+)$/.exec(identity);
  if (!parts || parts[2]!.length % 2 !== 0) return null;
  let source: string;
  try { source = new TextDecoder("utf-8", { fatal: true }).decode(hexToBytes(parts[2]!)); } catch { return null; }
  const sourceMatches = source ? messages.filter((message) => message.source_uuid === source) : [];
  if (sourceMatches.length === 1) return sourceMatches[0]!.ordinal;
  const candidates = source ? sourceMatches : messages;
  const matches = candidates.filter((message) => fingerprint(message) === parts[1]);
  return matches.length === 1 ? matches[0]!.ordinal : null;
}
