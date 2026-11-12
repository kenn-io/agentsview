<script lang="ts">
  import { onDestroy, untrack } from "svelte";
  import type { Session } from "../../api/types.js";
  import {
    SessionsService,
    type SessionToolSequencesResponse,
  } from "../../api/generated/index";
  import { ApiError, isAbortError } from "../../api/runtime.js";
  import { LatestRead } from "../../utils/latest-read.js";
  import { sessions } from "../../stores/sessions.svelte.js";
  import { messages as messagesStore } from "../../stores/messages.svelte.js";
  import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
  import SignalPanel from "../content/SignalPanel.svelte";
  import ToolSequencesPanel from "../content/ToolSequencesPanel.svelte";

  // The session health and tool-sequence panels the breadcrumb opens. The
  // breadcrumb mounts this only while the panels are open, so closing them
  // stops every read and forgets the pacing state.
  interface Props {
    session: Session;
  }

  let { session }: Props = $props();

  let toolSequencesData = $state<SessionToolSequencesResponse | null>(null);
  let toolSequencesLoading = $state(false);
  let toolSequencesFailed = $state(false);
  let toolSequencesUnavailable = $state(false);
  let toolSequencesRetry = $state(0);
  // The session, loaded message revision, end state and retry count the current or last read was for.
  let toolSequencesIdentity = "";
  let toolSequencesTimer: ReturnType<typeof setTimeout> | undefined;
  // The session the read clock and conflict count below belong to.
  let toolSequencesSessionId = "";
  // When the last read for that session began; 0 before the first read.
  let toolSequencesStartedAt = 0;
  // Reads in a row that a sync interrupted with a 409.
  let toolSequencesConflicts = 0;
  const toolSequencesRead = new LatestRead();

  $effect(() => {
    sessions.fetchSignalDetail(session.id);
  });

  // Every new message changes the transcript revision, and each read loads the whole session, so a busy session reads at most this often.
  const TOOL_SEQUENCES_MIN_INTERVAL_MS = 5_000;
  const TOOL_SEQUENCES_MAX_CONFLICTS = 3;

  function stopToolSequences() {
    toolSequencesRead.cancel();
    clearTimeout(toolSequencesTimer);
    toolSequencesTimer = undefined;
  }

  onDestroy(stopToolSequences);

  $effect(() => {
    const id = session.id;
    const termination = session.termination_status ?? "";
    const retry = toolSequencesRetry;

    if (untrack(() => toolSequencesData?.session_id !== id)) toolSequencesData = null;
    if (toolSequencesSessionId !== id) {
      toolSequencesSessionId = id;
      toolSequencesStartedAt = 0;
      toolSequencesConflicts = 0;
    }
    const loadedRevision = messagesStore.sessionId === id ? messagesStore.loadedRevision : null;
    if (loadedRevision === null && messagesStore.loading) {
      // The message list is replacing its rows; read once they land so the sequences match them.
      stopToolSequences();
      toolSequencesIdentity = "";
      toolSequencesLoading = true;
      toolSequencesFailed = false;
      toolSequencesUnavailable = false;
      return;
    }
    // A failed read keeps its identity, so only a new revision, end state or retry reads again.
    const identity = [id, loadedRevision ?? "", termination, retry].join("\n");
    if (identity === toolSequencesIdentity) return;
    toolSequencesIdentity = identity;
    stopToolSequences();
    toolSequencesLoading = true;
    toolSequencesFailed = false;
    toolSequencesUnavailable = false;
    const wait = toolSequencesStartedAt + TOOL_SEQUENCES_MIN_INTERVAL_MS - Date.now();
    if (toolSequencesStartedAt > 0 && wait > 0) {
      toolSequencesTimer = setTimeout(() => readToolSequences(id), wait);
    } else {
      readToolSequences(id);
    }
  });

  // Someone asked for the retry, so it skips the interval that paces automatic reads.
  function retryToolSequences() {
    toolSequencesStartedAt = 0;
    toolSequencesRetry++;
  }

  function readToolSequences(id: string) {
    toolSequencesTimer = undefined;
    toolSequencesStartedAt = Date.now();
    const signal = toolSequencesRead.begin();
    SessionsService.getApiV1SessionsByIdToolSequences({ id }, { signal })
      .then((response) => {
        if (!toolSequencesRead.finish(signal)) return;
        toolSequencesConflicts = 0;
        toolSequencesLoading = false;
        toolSequencesData = response;
      })
      .catch((error) => {
        if (isAbortError(error) || !toolSequencesRead.finish(signal)) return;
        const conflict = error instanceof ApiError && error.status === 409;
        if (conflict && ++toolSequencesConflicts < TOOL_SEQUENCES_MAX_CONFLICTS) {
          // A sync landed during the read; keep what is shown and read again after the interval.
          toolSequencesRetry++;
          return;
        }
        toolSequencesConflicts = 0;
        toolSequencesLoading = false;
        toolSequencesData = null;
        if (error instanceof ApiError && error.status === 501) {
          toolSequencesUnavailable = true;
        } else {
          toolSequencesFailed = true;
        }
      });
  }
</script>

<SignalPanel {session} />
<ToolSequencesPanel
  errorTelemetryKind="session"
  data={toolSequencesData?.session_id === session.id ? toolSequencesData : null}
  timing={sessionTiming.timing?.session_id === session.id ? sessionTiming.timing : null}
  sessionId={session.id}
  loading={toolSequencesLoading}
  failed={toolSequencesFailed}
  unavailable={toolSequencesUnavailable}
  onretry={retryToolSequences}
/>
