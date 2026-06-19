# Coach Heuristic Map for agentsview Session Quality

Phase: 1 planning artifact only.

This document maps AI Engineering Coach heuristics to agentsview-compatible
signal families and records which candidates are safe to use for deterministic
per-session scoring. It intentionally does not change runtime scoring, database
schema, APIs, or UI behavior.

Sources inventoried:

- `/tmp/AI-Engineering-Coach/src/core/rules/*.md` (45 built-in rules)
- `/tmp/AI-Engineering-Coach/src/core/detectors/scoring.ts`
- `/tmp/AI-Engineering-Coach/src/core/analyzer-insights.ts`
- Current agentsview signal code in `internal/signals` and
  `internal/sync/signal_compute.go`

## Current agentsview signal inputs

agentsview currently has deterministic access to:

- Session metadata: agent, project, machine, started/ended timestamps, message
  counts, output/context token summaries, automation flag, parent/relationship,
  cwd/git branch, termination status, and existing health signal columns.
- Message data: role, content, content length, timestamp, model, token usage,
  context/output tokens, system flag, source metadata, sidechain and compact
  boundary flags.
- Tool data: tool name, category, input JSON, skill name, result content,
  subagent session id, result event status, result timestamps, and event order.
- Usage data: model, input/output/cache/reasoning tokens, cost, source, and
  occurrence timestamp.

agentsview does not currently have reliable first-class request records,
explicit request cancellation, agent/plan mode, slash command, custom
instruction usage, tool confirmation/approval mode, request elapsed time, AI
generated LOC, human review timing, or parsed file-reference inventories.

## Signal Families

- `prompt_quality`: user prompt shape, constraints, success criteria,
  verification language, duplication, and task-start structure.
- `context_quality`: file/code context, context pressure, compactions, prompt
  cache behavior, and context-engineering habits.
- `workflow_hygiene`: session outcome, abandonment, drift, task length, timing,
  and loop behavior.
- `tool_mastery`: tool use, retries, failed tools, edit churn, MCP/skill usage,
  subagents, and command habits.
- `cost_model_hygiene`: model choice, cache economics, output verbosity, and
  premium/reasoning use.
- `aggregate_analytics`: cross-session, cross-project, or long-horizon habits
  that should not affect a single session score.

## Proposed Phase 2 Score Candidates

These are the only Coach-derived candidates that are initially safe enough for
per-session score consideration. Weights are guidance for later phases, not
implemented behavior.

| Proposed signal                  | Family             | Coach source                                      | Inputs available now                                            | Initial weight guidance | Confidence | Scoring notes                                                                                                                                                                                                                                                |
| -------------------------------- | ------------------ | ------------------------------------------------- | --------------------------------------------------------------- | ----------------------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `short_prompt_count`             | `prompt_quality`   | `lazy-prompting`                                  | User message content length and nearby assistant timing         | 2 points each, cap 6    | Low        | Count only short task starts or fresh work segments after a stale assistant response. Do not count conversational steering, acknowledgements, or short follow-ups immediately after an agent response. Keep small because terse expert prompts can be valid. |
| `constraintless_prompt_count`    | `prompt_quality`   | `low-constraint-usage`                            | User message content                                            | 2 points each, cap 6    | Medium     | Detect explicit constraint language. Do not penalize read-only Q&A sessions unless task intent suggests implementation/debugging.                                                                                                                            |
| `missing_success_criteria_count` | `prompt_quality`   | Coach prompt maturity analyzer                    | User message content                                            | 2 points each, cap 6    | Medium-low | Use as a weak prompt-quality signal only for implementation/debugging task starts.                                                                                                                                                                           |
| `missing_verification_count`     | `prompt_quality`   | Coach prompt maturity analyzer                    | User message content                                            | 2 points each, cap 6    | Medium-low | Penalize only when the user is asking for code or behavior changes.                                                                                                                                                                                          |
| `unstructured_start`             | `prompt_quality`   | `no-spec-structure`, `no-spec-driven-development` | First user message, message count, tool/edit evidence           | 6 points once           | Medium     | Session-level flag for multi-turn agentic/code sessions whose first prompt lacks headings, bullets, numbered steps, spec keywords, constraints, or plan language.                                                                                            |
| `duplicate_prompt_count`         | `prompt_quality`   | `repeated-prompts`                                | Normalized user message content                                 | 3 points each, cap 9    | High       | Exact or near-exact repeated prompts inside one session are a strong signal of unclear progress.                                                                                                                                                             |
| `no_code_context_count`          | `context_quality`  | `no-file-context`                                 | User message content plus tool/edit evidence                    | 3 points each, cap 6    | Medium-low | Only score when code-task intent is detectable. General questions and planning conversations must be excluded.                                                                                                                                               |
| `runaway_tool_loop_count`        | `workflow_hygiene` | `runaway-agent-loops`                             | Tool calls grouped between user turns or across session windows | 5 points, cap 5         | Medium     | Compatibility field name; operationally this means repeated failing tool cycles. Require repeated failures so long productive harness/model runs are not penalized.                                                                                          |

Recommended Phase 2 total cap for all new prompt/content-derived penalties: 20
points. This keeps the existing outcome/tool/context score dominant while new
prompt-quality signals are calibrated.

## Already Covered by Existing Score

| Coach heuristic                                           | Existing agentsview signal                                     | Family                | Score surface           | Notes                                                                                                                                                  |
| --------------------------------------------------------- | -------------------------------------------------------------- | --------------------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `abandon-sessions`                                        | `outcome_abandoned`                                            | `workflow_hygiene`    | Already covered score   | agentsview classifies ended-with-user sessions as abandoned with confidence gates. Coach's aggregate abandoned-session rate can remain analytics-only. |
| `high-cancellation`                                       | `tool_failure_signals` for cancelled tool result events        | `workflow_hygiene`    | Partially covered score | Request cancellation is unavailable. Do not add a cancellation penalty until explicit request cancellation exists.                                     |
| `mega-sessions`                                           | `compactions`, `mid_task_compactions`, `context_pressure_high` | `context_quality`     | Already covered score   | Long sessions alone are not necessarily poor. Current context-pressure signals capture the risky part.                                                 |
| Coach scoring detector: canceled request penalty          | Existing tool result cancellation only                         | `workflow_hygiene`    | Partially covered score | Explicit cancelled requests are missing.                                                                                                               |
| Coach scoring detector: short prompt penalty              | `short_prompt_count` candidate                                 | `prompt_quality`      | New score candidate     | Use a smaller cap than Coach's weekly score because agentsview scores individual sessions.                                                             |
| Coach scoring detector: no file refs penalty              | `no_code_context_count` candidate                              | `context_quality`     | New score candidate     | Must be intent-gated.                                                                                                                                  |
| Coach scoring detector: late-night/weekend penalties      | Time heatmaps and aggregate analytics                          | `aggregate_analytics` | Analytics only          | Do not score a session because it occurred at a particular hour or weekday.                                                                            |
| Coach scoring detector: speed accept penalty              | Blocked until review timing and AI LOC exist                   | `tool_mastery`        | Excluded                | See blocked table.                                                                                                                                     |
| Coach scoring detector: no tools/no slash command penalty | Tool mix analytics and skills data                             | `tool_mastery`        | Insights/analytics only | Not a per-session quality failure by itself.                                                                                                           |

## Coach Rule Mapping

`Adoption` values:

- `score`: suitable for deterministic per-session scoring after Phase 2
  implementation.
- `aggregate`: useful for aggregate analytics only.
- `insights`: useful for generated or surfaced insights, not score.
- `covered`: already represented by existing agentsview scoring or analytics.
- `blocked`: exclude until required data exists or false-positive risk is
  reduced.

| Coach rule                      | Coach group     | agentsview family     | Proposed agentsview mapping                                     | Adoption  | Confidence | Required inputs and notes                                                                                                                                                                                                                                                             |
| ------------------------------- | --------------- | --------------------- | --------------------------------------------------------------- | --------- | ---------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `abandon-sessions`              | session-hygiene | `workflow_hygiene`    | `outcome_abandoned`; aggregate abandoned-session rate           | covered   | High       | Existing outcome classification covers per-session abandonment. Aggregate rate can be reported without new score logic.                                                                                                                                                               |
| `agent-mode-for-asks`           | prompt-quality  | `cost_model_hygiene`  | `simple_question_agent_mode_rate`                               | blocked   | Low        | Needs explicit agent mode, request-level code output, request cancellation, and reliable simple-question classification. Misleading as score.                                                                                                                                         |
| `agentic-no-tools`              | tool-mastery    | `tool_mastery`        | `agentic_no_tool_request_rate`                                  | insights  | Low        | Needs explicit agent mode. A no-tool agent turn can be valid for planning or explanation.                                                                                                                                                                                             |
| `auto-approve-terminal`         | code-review     | `tool_mastery`        | `auto_approved_terminal_count`                                  | blocked   | Low        | Needs approval/confirmation mode metadata not currently stored. Security signal, not score until data is exact.                                                                                                                                                                       |
| `auto-avoidance`                | tool-mastery    | `cost_model_hygiene`  | `manual_model_selection_rate`, `auto_model_absence`             | blocked   | Low        | Needs explicit auto-model availability/selection metadata. Current model mix alone cannot prove avoidance.                                                                                                                                                                            |
| `broken-flow-state`             | session-hygiene | `aggregate_analytics` | `low_score_rapid_followup_rate`                                 | aggregate | Medium     | Cross-day habit based on follow-up timing and low scores. Do not penalize individual sessions.                                                                                                                                                                                        |
| `cache-hit-starvation`          | tool-mastery    | `cost_model_hygiene`  | `prompt_cache_starvation_rate`                                  | aggregate | Medium     | Usage events include cache-read and input-token fields. Keep as cost analytics, not quality score.                                                                                                                                                                                    |
| `caps-lock`                     | prompt-quality  | `prompt_quality`      | `caps_prompt_count`                                             | insights  | Medium     | Potential frustration signal, but subjective and culturally noisy. No per-session penalty.                                                                                                                                                                                            |
| `context-engineering-gaps`      | prompt-quality  | `context_quality`     | `context_engineering_gap_summary`                               | insights  | Medium-low | Mixed feature inventory across file refs, instructions, skills, MCP, and subagents. Several inputs are incomplete today.                                                                                                                                                              |
| `copy-paste-blindness`          | code-review     | `tool_mastery`        | `ai_loc_without_refinement_count`                               | blocked   | Low        | Needs AI-generated LOC and review/refinement evidence. Not available.                                                                                                                                                                                                                 |
| `excessive-file-context`        | prompt-quality  | `context_quality`     | `excessive_file_context_rate`                                   | aggregate | Low        | Requires structured referenced-file counts. Tool read/search calls are only a weak proxy and should not score.                                                                                                                                                                        |
| `frustration-signals`           | prompt-quality  | `prompt_quality`      | `frustration_marker_count`                                      | insights  | Medium     | Useful as a "session may be going badly" marker for drilldowns and generated insight copy, but not a deterministic score penalty. Tune for precision over recall. Strip code/log blocks, ignore ordinary emphasis, and treat profanity/caps as contextual evidence rather than proof. |
| `high-cancellation`             | session-hygiene | `workflow_hygiene`    | `request_cancellation_rate`                                     | blocked   | Low        | Explicit request cancellation is unavailable. Tool cancellation remains covered by failure signals.                                                                                                                                                                                   |
| `instruction-bloat`             | prompt-quality  | `context_quality`     | `instruction_context_bytes`                                     | aggregate | Low        | Needs reliable custom/system instruction extraction by source. Avoid per-session score.                                                                                                                                                                                               |
| `late-night-coding`             | session-hygiene | `aggregate_analytics` | `late_night_session_rate`                                       | aggregate | High       | Timestamps are available. This is a work-pattern insight, not a session-quality penalty.                                                                                                                                                                                              |
| `lazy-prompting`                | prompt-quality  | `prompt_quality`      | `short_prompt_count`                                            | score     | Medium     | Available from user message length. Keep capped and intent-aware.                                                                                                                                                                                                                     |
| `low-constraint-usage`          | prompt-quality  | `prompt_quality`      | `constraintless_prompt_count`                                   | score     | Medium     | Available from prompt text. Use conservative regexes and low cap.                                                                                                                                                                                                                     |
| `low-markdown-ratio`            | prompt-quality  | `aggregate_analytics` | `documentation_output_ratio`                                    | blocked   | Low        | Needs reliable output file extension/LOC extraction. Code block language alone is insufficient.                                                                                                                                                                                       |
| `mcp-tool-bloat`                | tool-mastery    | `tool_mastery`        | `distinct_tool_count`, `distinct_mcp_tool_count`                | aggregate | Medium     | Tool names are available. High tool count can reflect complex work, so aggregate only.                                                                                                                                                                                                |
| `mega-sessions`                 | session-hygiene | `context_quality`     | `long_session_count`; existing context signals                  | covered   | High       | Session length is available, but score should stay tied to compaction/context pressure.                                                                                                                                                                                               |
| `model-overreliance`            | tool-mastery    | `cost_model_hygiene`  | `top_model_share`, `model_diversity`                            | aggregate | High       | Model mix exists. This is portfolio analytics, not per-session score.                                                                                                                                                                                                                 |
| `no-custom-instructions`        | tool-mastery    | `context_quality`     | `custom_instruction_absence_rate`                               | blocked   | Low        | No explicit custom-instruction usage field. System messages are not a reliable substitute.                                                                                                                                                                                            |
| `no-devcontainer`               | code-review     | `tool_mastery`        | `unsandboxed_terminal_rate`                                     | blocked   | Low        | Needs runtime/container/sandbox metadata. Do not infer from Bash usage.                                                                                                                                                                                                               |
| `no-file-context`               | prompt-quality  | `context_quality`     | `no_code_context_count`                                         | score     | Medium-low | Score only for detectable code tasks. Exclude general Q&A, ideation, and pure planning.                                                                                                                                                                                               |
| `no-language-exploration`       | code-review     | `aggregate_analytics` | `new_language_weekly_count`                                     | blocked   | Low        | Needs parsed language inventory from user/assistant code or changed files. Not a session score.                                                                                                                                                                                       |
| `no-plan-mode`                  | tool-mastery    | `tool_mastery`        | `planning_mode_usage_rate`                                      | blocked   | Low        | Needs explicit plan-mode metadata. Prompt keywords are too weak for scoring.                                                                                                                                                                                                          |
| `no-skills`                     | tool-mastery    | `tool_mastery`        | `skill_usage_rate`, `distinct_skill_count`                      | aggregate | Medium     | `tool_calls.skill_name` is available for some agents. Absence is not a per-session failure.                                                                                                                                                                                           |
| `no-slash-commands`             | tool-mastery    | `tool_mastery`        | `slash_command_usage_rate`                                      | blocked   | Low        | Slash command metadata is not currently parsed. Not a score candidate.                                                                                                                                                                                                                |
| `no-spec-driven-development`    | prompt-quality  | `prompt_quality`      | `spec_driven_session_rate`; contributes to `unstructured_start` | aggregate | Medium     | Aggregate habit is useful. Per-session scoring should use only the narrower task-start structure signal.                                                                                                                                                                              |
| `no-spec-structure`             | prompt-quality  | `prompt_quality`      | `unstructured_start`                                            | score     | Medium     | Safe when gated to multi-turn code/agentic sessions.                                                                                                                                                                                                                                  |
| `premium-for-lookup-questions`  | tool-mastery    | `cost_model_hygiene`  | `premium_lookup_question_rate`                                  | insights  | Medium-low | Model and cost data exist, but "lookup question" classification is brittle. Use cost insight, not score.                                                                                                                                                                              |
| `premium-waste`                 | tool-mastery    | `cost_model_hygiene`  | `premium_short_prompt_rate`                                     | insights  | Medium-low | Cost insight only. Short prompts can be legitimate with rich prior context.                                                                                                                                                                                                           |
| `profanity`                     | prompt-quality  | `prompt_quality`      | `hostile_language_count`                                        | insights  | Medium     | Personal tone should not reduce deterministic session score.                                                                                                                                                                                                                          |
| `reasoning-effort-overuse`      | tool-mastery    | `cost_model_hygiene`  | `high_reasoning_effort_rate`                                    | blocked   | Low        | Usage events expose reasoning tokens, not requested effort setting. Keep out until effort metadata exists.                                                                                                                                                                            |
| `repeated-prompts`              | prompt-quality  | `prompt_quality`      | `duplicate_prompt_count`                                        | score     | High       | Normalize text and count duplicates within a session. Strong signal when repeated after assistant/tool work.                                                                                                                                                                          |
| `runaway-agent-loops`           | session-hygiene | `workflow_hygiene`    | `runaway_tool_loop_count`                                       | score     | Medium     | Tool calls are available, but duration alone is not a failure. Count only repeated failing cycles or dense failure windows.                                                                                                                                                           |
| `session-drift`                 | session-hygiene | `workflow_hygiene`    | `session_intent_drift_count`                                    | insights  | Low        | Requires reliable work-type classification. Useful for insight summaries, not score.                                                                                                                                                                                                  |
| `slow-responses`                | session-hygiene | `workflow_hygiene`    | `slow_response_count`                                           | blocked   | Low        | Request elapsed time is unavailable. Session duration is not equivalent.                                                                                                                                                                                                              |
| `speed-accept`                  | code-review     | `tool_mastery`        | `speed_accept_count`                                            | blocked   | Low        | Needs AI LOC plus human review/accept timing. Not available.                                                                                                                                                                                                                          |
| `tunnel-vision`                 | code-review     | `aggregate_analytics` | `top_project_share`                                             | aggregate | High       | Project distribution is available. This is portfolio analytics only.                                                                                                                                                                                                                  |
| `verbose-output`                | prompt-quality  | `cost_model_hygiene`  | `verbose_output_rate`                                           | insights  | Medium     | Output tokens are available. Avoid score because verbosity can be requested or useful.                                                                                                                                                                                                |
| `verbose-prompt-no-compression` | prompt-quality  | `prompt_quality`      | `verbose_uncompressed_prompt_rate`                              | insights  | Low        | Compression-skill detection is incomplete and verbose prompts can be deliberate.                                                                                                                                                                                                      |
| `vibe-coding`                   | code-review     | `tool_mastery`        | `large_ai_loc_low_prompt_count`                                 | blocked   | Low        | Needs AI LOC and review evidence. Not available.                                                                                                                                                                                                                                      |
| `weekend-overwork`              | session-hygiene | `aggregate_analytics` | `weekend_session_rate`                                          | aggregate | High       | Timestamps are available. Work-life analytics only.                                                                                                                                                                                                                                   |
| `yolo-mode`                     | code-review     | `tool_mastery`        | `auto_approval_rate`                                            | blocked   | Low        | Needs explicit approval/confirmation metadata. Do not infer from Bash or Edit calls.                                                                                                                                                                                                  |

## Coach Insights Analyzer Mapping

| Coach analyzer area        | agentsview mapping                                                                                                                                              | Adoption                                 | Notes                                                                                                              |
| -------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| Intent classification      | `session_intent`, `intent_distribution`                                                                                                                         | insights                                 | Can be keyword-assisted, but should not affect deterministic score until validated.                                |
| Spec-driven development    | `spec_driven_session_rate`, `unstructured_start`                                                                                                                | aggregate plus score sub-signal          | Aggregate rate is useful. Only the narrow `unstructured_start` flag belongs in scoring.                            |
| Production vs review ratio | `ai_loc_review_ratio`                                                                                                                                           | blocked                                  | Needs AI LOC and review gap data.                                                                                  |
| Sustainable pace           | late-night/weekend/streak analytics                                                                                                                             | aggregate                                | Good analytics and insight material, explicitly excluded from score.                                               |
| Prompt maturity            | constraints, success criteria, verification, context provision, specificity dimensions                                                                          | insights plus selected score sub-signals | Use dimensions for insights. Only simple deterministic absence counts are score candidates, capped conservatively. |
| Migration readiness        | feature usage matrix for subagents, MCP, instructions, plan mode, skills, slash commands, multi-file edits, terminal access, file references, parallel sessions | insights                                 | Several fields are partial or agent-specific. Good for adoption insights, not quality score.                       |

## Blocked Data Fields

The following fields would be needed before adopting currently blocked rules:

- Request boundary model: first-class user request records with associated
  assistant response, tool calls, elapsed time, cancellation, and generated code
  artifacts.
- Mode metadata: agent mode, plan mode, auto-model setting, slash command, and
  requested reasoning effort.
- Safety metadata: tool confirmation prompts, auto-approval state, yolo mode,
  sandbox/devcontainer state, and terminal approval source.
- Context metadata: parsed file references, custom instruction provenance,
  instruction byte counts, and explicit MCP/tool catalog size.
- Code/review metadata: AI-generated LOC, edited LOC, accepted change timing,
  human follow-up review gaps, changed file extensions, and code language
  inventory.

Until these fields exist, the affected Coach rules should remain excluded from
per-session scoring.

## Implementation Guidance for Later Phases

1. Add prompt/content scorers as pure functions under `internal/signals`, taking
   already-loaded messages and tool calls as inputs.
1. Keep score computation deterministic and local. Do not call LLMs for scoring.
1. Gate prompt-quality penalties to code-task or multi-turn task sessions where
   possible.
1. Preserve existing health score semantics by capping all new Coach-derived
   prompt/context penalties at 20 total points.
1. Store aggregate-only heuristics separately from score-affecting session
   signals so analytics and health scores do not drift together.
1. When parser changes introduce new required fields, follow the repository data
   safety rule: full resync through a fresh database, orphan session copy, and
   atomic swap; do not delete or recreate the persistent archive in place.

## Current Weak Signal Semantics

Some Coach-derived prompt signals are intentionally weak and should be used as
evidence pivots, not as standalone conclusions:

- `short_prompt_count` means a short task-start or fresh work-segment prompt. It
  excludes controls such as "yes" and short conversational steering after a
  recent assistant response. A short prompt can still be expert shorthand when
  the surrounding session supplies enough context.
- `frustration_marker_count` means a user message contains high-precision
  frustration language after stripping fenced code/log-like payloads. It is a
  workflow triage marker, not a user-quality or session-score penalty.
- `runaway_tool_loop_count` means repeated failing tool cycles. Long productive
  harness runs, model work loops, or uninterrupted agent sessions are not
  failures by duration alone.
