import type { Session } from "../../api/types.js";
import {
  OpenersService,
  SessionsService,
  type Opener,
  type ResumeRequest,
} from "../../api/generated/index";
import { isAbortError } from "../../api/runtime.js";
import { m } from "../../i18n/index.js";
import { messages as messagesStore } from "../../stores/messages.svelte.js";
import { claudeCodeLink } from "../../utils/claude.js";
import { copyToClipboard } from "../../utils/clipboard.js";
import { codexDesktopLink } from "../../utils/codex.js";
import { LatestRead } from "../../utils/latest-read.js";
import {
  buildResumeCommand,
  formatResumeResponseCommand,
  supportsResume,
} from "../../utils/resume.js";

interface SessionDirectoryResponse {
  path: string;
}

/**
 * State and actions behind the session header's Resume/Open menu: the
 * openers this machine offers, the session's directory, and the feedback
 * the trigger shows after an action. The header renders the menu in one of
 * two places depending on its width, so the state lives here rather than in
 * the menu component, and survives the move. Create it while a component
 * initializes; its reads stop when that component is destroyed.
 */
export class SessionOpeners {
  menuOpen = $state(false);
  openers: Opener[] = $state([]);
  feedback = $state("");
  feedbackKind = $state<"success" | "error">("success");
  sessionDir = $state<string | null>(null);

  private feedbackTimer: ReturnType<typeof setTimeout> | undefined;
  private readonly openersRead = new LatestRead();
  private readonly directoryRead = new LatestRead();
  private resolvedSessionDirId: string | null = null;
  private pendingSessionDirId: string | null = null;
  private readonly getSession: () => Session | undefined;

  constructor(getSession: () => Session | undefined) {
    this.getSession = getSession;
    $effect(() => this.readOpeners());
    $effect(() => this.readDirectory());
    $effect(() => () => {
      this.openersRead.cancel();
      this.directoryRead.cancel();
    });
  }

  private readonly session = $derived.by(() => this.getSession());

  // Remote sessions have host-prefixed IDs (host~rawID).
  readonly isLocal = $derived(!this.session?.id.includes("~"));

  readonly canLaunch = $derived(
    this.session ? supportsResume(this.session.agent) && this.isLocal : false,
  );

  readonly codexLink = $derived(
    this.session ? codexDesktopLink(this.session.agent, this.session.id) : null,
  );

  readonly claudeLink = $derived(
    this.session?.agent === "claude" && this.isLocal ? claudeCodeLink(this.sessionDir) : null,
  );

  readonly canCopyCommand = $derived(this.session ? supportsResume(this.session.agent) : false);

  readonly terminalOpeners = $derived(this.openers.filter((o) => o.kind === "terminal"));

  readonly claudeDesktopOpener = $derived(
    this.session?.agent === "claude"
      ? (this.openers.find((o) => o.id === "claude-desktop") ?? null)
      : null,
  );

  readonly editorOpeners = $derived(this.openers.filter((o) => o.kind === "editor"));

  readonly fileOpeners = $derived(this.openers.filter((o) => o.kind === "files"));

  readonly showDropdown = $derived(
    this.canCopyCommand ||
      this.codexLink !== null ||
      (this.isLocal &&
        (this.editorOpeners.length > 0 ||
          this.fileOpeners.length > 0 ||
          (this.sessionDir !== null && !!this.session?.file_path))),
  );

  private readOpeners() {
    const signal = this.openersRead.begin();
    OpenersService.getApiV1Openers({ signal })
      .then((res) => {
        if (!this.openersRead.isCurrent(signal)) return;
        this.openers = res.openers;
      })
      .catch((e) => {
        if (!isAbortError(e)) this.openers = [];
      })
      .finally(() => this.openersRead.finish(signal));
  }

  private readDirectory() {
    const session = this.getSession();
    if (!session) {
      this.directoryRead.cancel();
      this.sessionDir = null;
      this.resolvedSessionDirId = null;
      this.pendingSessionDirId = null;
      return;
    }
    const id = session.id;
    if (id === this.resolvedSessionDirId || id === this.pendingSessionDirId) return;
    const signal = this.directoryRead.begin();
    this.pendingSessionDirId = id;
    this.sessionDir = null;
    SessionsService.getApiV1SessionsByIdDirectory({ id }, { signal })
      .then(({ path }) => {
        if (this.getSession()?.id === id && this.directoryRead.isCurrent(signal)) {
          this.sessionDir = (path as SessionDirectoryResponse["path"]) || null;
          this.resolvedSessionDirId = id;
        }
      })
      .catch((e) => {
        if (isAbortError(e)) return;
        // Don't cache the ID on failure so the next
        // session refresh retries the lookup.
      })
      .finally(() => {
        if (this.directoryRead.finish(signal) && this.pendingSessionDirId === id) {
          this.pendingSessionDirId = null;
        }
      });
  }

  private showFeedback(msg: string, kind: "success" | "error" = "success") {
    this.feedback = msg;
    this.feedbackKind = kind;
    clearTimeout(this.feedbackTimer);
    this.feedbackTimer = setTimeout(() => {
      this.feedback = "";
    }, 2000);
  }

  // Copies the resume command built in the browser, for when the server
  // could neither launch nor return one. The model comes from the session
  // the request was for, even if another session opened while it ran.
  private async copyLocalResumeCommand(session: Session) {
    const cmd = buildResumeCommand(session.agent, session.id, {
      model: messagesStore.resumeModelFor(session.id),
    });
    if (cmd) {
      const ok = await copyToClipboard(cmd);
      this.showFeedback(
        ok ? m.session_breadcrumb_command_copied() : m.session_breadcrumb_failed(),
        ok ? "success" : "error",
      );
    } else {
      this.showFeedback(m.session_breadcrumb_not_supported(), "error");
    }
  }

  async resumeIn(opener: Opener) {
    const session = this.getSession();
    if (!session) return;
    this.menuOpen = false;
    try {
      const resp = await SessionsService.postApiV1SessionsByIdResume({ id: session.id }, {
        opener_id: opener.id,
      } satisfies ResumeRequest);
      if (resp.launched) {
        this.showFeedback(
          m.session_breadcrumb_resumed_in({
            target: resp.terminal ?? opener.name,
          }),
        );
        return;
      }
      // Launch failed — fall back to clipboard copy.
      if (resp.command) {
        const cmd = formatResumeResponseCommand(session.agent, resp);
        const ok = cmd ? await copyToClipboard(cmd) : false;
        this.showFeedback(
          ok ? m.session_breadcrumb_command_copied() : m.session_breadcrumb_failed(),
          ok ? "success" : "error",
        );
        return;
      }
    } catch {
      // Fall back to local command build.
    }
    await this.copyLocalResumeCommand(session);
  }

  async copyResumeCommand() {
    const session = this.getSession();
    if (!session) return;
    this.menuOpen = false;
    try {
      const resp = await SessionsService.postApiV1SessionsByIdResume({ id: session.id }, {
        command_only: true,
      } satisfies ResumeRequest);
      if (resp.command) {
        const cmd = formatResumeResponseCommand(session.agent, resp);
        const ok = cmd ? await copyToClipboard(cmd) : false;
        this.showFeedback(
          ok ? m.session_breadcrumb_command_copied() : m.session_breadcrumb_failed(),
          ok ? "success" : "error",
        );
        return;
      }
    } catch {
      // Fall back to local build.
    }
    await this.copyLocalResumeCommand(session);
  }

  async copyFilePath() {
    this.menuOpen = false;
    if (!this.sessionDir) {
      this.showFeedback(m.session_breadcrumb_no_path_available(), "error");
      return;
    }
    const ok = await copyToClipboard(this.sessionDir);
    this.showFeedback(
      ok ? m.session_breadcrumb_path_copied() : m.session_breadcrumb_failed(),
      ok ? "success" : "error",
    );
  }

  async openIn(opener: Opener) {
    const session = this.getSession();
    if (!session) return;
    this.menuOpen = false;
    try {
      await SessionsService.postApiV1SessionsByIdOpen({ id: session.id }, { opener_id: opener.id });
      this.showFeedback(
        m.session_breadcrumb_opened_in({
          target: opener.name,
        }),
      );
    } catch {
      this.showFeedback(m.session_breadcrumb_failed_to_open(), "error");
    }
  }

  async resumeDefault() {
    const session = this.getSession();
    if (!session) return;
    this.menuOpen = false;
    try {
      const resp = await SessionsService.postApiV1SessionsByIdResume({ id: session.id }, {});
      if (resp.launched) {
        this.showFeedback(
          m.session_breadcrumb_resumed_in({
            target: resp.terminal ?? "terminal",
          }),
        );
        return;
      }
      if (resp.command) {
        const cmd = formatResumeResponseCommand(session.agent, resp);
        const ok = cmd ? await copyToClipboard(cmd) : false;
        this.showFeedback(
          ok ? m.session_breadcrumb_command_copied() : m.session_breadcrumb_failed(),
          ok ? "success" : "error",
        );
        return;
      }
    } catch {
      // Fall back to local command build.
    }
    await this.copyLocalResumeCommand(session);
  }
}
