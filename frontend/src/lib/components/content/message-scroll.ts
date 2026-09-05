const LATEST_EDGE_TOLERANCE_PX = 8;

export interface ScrollMetrics {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

export type ScrollAlign = "start" | "end";

export function getAlignedOffsetScrollAlign(_align: ScrollAlign): "start" {
  return "start";
}

export function getLatestDisplayIndex(displayCount: number, newestFirst: boolean): number {
  if (displayCount <= 0) return -1;
  return newestFirst ? 0 : displayCount - 1;
}

export function isAtLatestEdge(metrics: ScrollMetrics, newestFirst: boolean): boolean {
  if (newestFirst) {
    return metrics.scrollTop <= LATEST_EDGE_TOLERANCE_PX;
  }

  return (
    metrics.scrollHeight - metrics.clientHeight - metrics.scrollTop <= LATEST_EDGE_TOLERANCE_PX
  );
}
