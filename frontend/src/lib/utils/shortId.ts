export function shortenId(id: string, peers: string[]): string {
  const head = id.slice(0, 8);
  if (peers.every((peer) => peer === id || peer.slice(0, 8) !== head)) return head;
  const tail = id.slice(-8);
  if (peers.every((peer) => peer === id || peer.slice(-8) !== tail)) return `…${tail}`;
  return id;
}
