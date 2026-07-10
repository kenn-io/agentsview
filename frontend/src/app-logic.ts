import {
  filterParamsEqual,
  sessionDateIntentCleared,
  sessionRouteParamsForDetailExit,
  sessionRouteParamsForFilters,
} from "./lib/stores/sessionRouteParams.js";

export interface ReadProgressBaselineState {
  activeSessionId: string | null;
  messageSessionId: string | null;
  loading: boolean;
  initialLoadSucceeded: boolean;
  latestDisplayOrdinal: number | null | undefined;
}

export function shouldBaselineReadProgress(
  state: ReadProgressBaselineState,
): boolean {
  return !!state.activeSessionId &&
    state.activeSessionId === state.messageSessionId &&
    !state.loading &&
    state.initialLoadSucceeded &&
    state.latestDisplayOrdinal !== undefined;
}

type SessionRouteActionBase = {
  clearYoke: boolean;
};

export type SessionRouteSyncAction =
  | ({
      kind: "reset-signature";
      nextSignature: null;
    } & SessionRouteActionBase)
  | ({
      kind: "none";
      nextSignature: string | null;
    } & SessionRouteActionBase)
  | ({
      kind: "replace-params";
      nextParams: Record<string, string>;
      nextSignature: string;
    } & SessionRouteActionBase)
  | ({
      kind: "navigate-to-session";
      sessionId: string;
      nextParams: Record<string, string>;
      nextSignature: string;
    } & SessionRouteActionBase)
  | ({
      kind: "navigate-from-session";
      nextParams: Record<string, string>;
      nextSignature: null;
    } & SessionRouteActionBase);

export interface SessionRouteSyncState {
  route: string;
  activeSessionId: string | null;
  currentUrlSessionId: string | null;
  filterParams: Record<string, string>;
  currentParams: Record<string, string>;
  lastDetailFilterParamsSignature: string | null;
  now?: Date;
}

export function resolveSessionRouteSync(
  state: SessionRouteSyncState,
): SessionRouteSyncAction {
  const {
    route,
    activeSessionId,
    currentUrlSessionId,
    filterParams,
    currentParams,
    lastDetailFilterParamsSignature,
    now,
  } = state;
  const filterParamsSignature = JSON.stringify(filterParams);

  if (route !== "sessions") {
    return {
      kind: "reset-signature",
      nextSignature: null,
      clearYoke: false,
    };
  }

  if (activeSessionId) {
    const nextParams = sessionRouteParamsForFilters(
      filterParams,
      currentParams,
      now,
    );

    if (activeSessionId === currentUrlSessionId) {
      if (
        lastDetailFilterParamsSignature !== null &&
        lastDetailFilterParamsSignature !== filterParamsSignature &&
        !filterParamsEqual(currentParams, nextParams)
      ) {
        return {
          kind: "replace-params",
          nextParams,
          nextSignature: filterParamsSignature,
          clearYoke: sessionDateIntentCleared(currentParams, nextParams),
        };
      }

      return {
        kind: "none",
        nextSignature: filterParamsSignature,
        clearYoke: false,
      };
    }

    return {
      kind: "navigate-to-session",
      sessionId: activeSessionId,
      nextParams,
      nextSignature: filterParamsSignature,
      clearYoke: sessionDateIntentCleared(currentParams, nextParams),
    };
  }

  if (currentUrlSessionId === null) {
    return {
      kind: "reset-signature",
      nextSignature: null,
      clearYoke: false,
    };
  }

  const filterChangedOnDetail =
    lastDetailFilterParamsSignature !== null &&
    lastDetailFilterParamsSignature !== filterParamsSignature;
  const nextParams = filterChangedOnDetail
    ? sessionRouteParamsForFilters(filterParams, currentParams, now)
    : sessionRouteParamsForDetailExit(filterParams, currentParams);

  return {
    kind: "navigate-from-session",
    nextParams,
    nextSignature: null,
    clearYoke: sessionDateIntentCleared(currentParams, nextParams),
  };
}

export type SessionRouteWriteBackAction =
  | {
      kind: "none";
      clearYoke: false;
    }
  | {
      kind: "replace-params";
      nextParams: Record<string, string>;
      clearYoke: boolean;
    };

export interface SessionRouteWriteBackState {
  route: string;
  currentUrlSessionId: string | null;
  filterParams: Record<string, string>;
  currentParams: Record<string, string>;
  now?: Date;
}

export function resolveSessionRouteWriteBack(
  state: SessionRouteWriteBackState,
): SessionRouteWriteBackAction {
  const {
    route,
    currentUrlSessionId,
    filterParams,
    currentParams,
    now,
  } = state;

  if (route !== "sessions" || currentUrlSessionId !== null) {
    return {
      kind: "none",
      clearYoke: false,
    };
  }

  const nextParams = sessionRouteParamsForFilters(
    filterParams,
    currentParams,
    now,
  );
  if (filterParamsEqual(currentParams, nextParams)) {
    return {
      kind: "none",
      clearYoke: false,
    };
  }

  return {
    kind: "replace-params",
    nextParams,
    clearYoke: sessionDateIntentCleared(currentParams, nextParams),
  };
}

export function getLastRecentlyDeletedBatch<T>(
  batches: readonly T[],
): T | null {
  return batches.at(-1) ?? null;
}
