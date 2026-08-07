import type { Message } from "../api/types.js";

const incident = /\b(?:root cause|failed|failures?|failing|blocked|broken|timed out|error|issue|stopped\s+(?:before|after|because|at|on|by))\b|\bhit\s+(?:a|an|the)?\s*(?:(?:local|powershell|shell|argument|tool)\s+){0,4}(?:bug|error|failure)\b/i;
const locality = /\b(?:powershell|crlf|psql|ensurepip|virtualenv|tsx|runner\.dll|puppeteer|libssl|ladybugdb|\.gitnexus|isolated-container)\b|\bgitnexus.{0,90}\b(?:index|graph|fts|generated|shadow|refresh)\b|\bshell\s+policy\b|\bcommand[- ]policy\b|\bin-app\s+browser.{0,50}network[- ]isolated\b|\bworktree.{0,80}(?:permission|root[- ]owned)\b|\/opt.{0,60}(?:permission|root)|\/tmp.{0,50}compose|\buntracked.{0,80}(?:fixture|xml)\b|\btemporary\s+executable\b|\bdocker\s+inspection\b/i;
const containment = /\b(?:no|not|without)\b[^.]{0,80}\b(?:production|prod|live|server|service|database|application|fleet|state|source|data)\b[^.]{0,60}\b(?:changed|touched|affected|started|contacted)\b|\b(?:production|prod|live|server|service|database|application|fleet|state|source|data)\b[^.]{0,60}\b(?:unchanged|untouched|unaffected)\b|\b(?:local[- ]only|non-product|temporary|isolated|disposable)\b|\b(?:repairing|fixing|changing|creating)\s+only\b|\bwill\s+not\s+rerun\b/i;
const recovery = /\b(?:validate|validating|fix|fixing|repair|repairing|rerun|rerunning|resume|resuming|change|changing|create|creating|reconcile|reconciling|isolate|isolating|collect|collecting|make|making|continue|continuing|record|recording|run|running)\b/i;
const successOutcome = /^(?:production|deployment|release|the new web container|the application fix|offline validation|cross-owner access|the targeted behavior tests|the dry-run reconciliation|gitnexus rebuilt successfully|the clean release image built successfully|the disposable probe completed)\b/i;
const negatedIncident = /\b(?:no|not|without)\b.{0,40}\b(?:failures?|errors?|issues?|blocked|broken|timed\s+out)\b/i;
const heartbeat = /\b(?:still\s+running|remains\s+in\s+progress|has\s+not\s+returned|no\s+reason\s+to\s+intervene)\b/i;

export function isHighConfidenceLocalRemediation(
  content: string,
): boolean {
  return incident.test(content) &&
    locality.test(content) &&
    containment.test(content) &&
    recovery.test(content) &&
    !successOutcome.test(content) &&
    !negatedIncident.test(content) &&
    !heartbeat.test(content);
}

export function shouldHideResolvedRemediation(
  message: Pick<Message, "content" | "role" | "source_subtype">,
  outcome: string | undefined,
  enabled: boolean,
): boolean {
  return enabled &&
    outcome === "completed" &&
    message.role === "assistant" &&
    message.source_subtype === "commentary" &&
    isHighConfidenceLocalRemediation(message.content);
}

export function countResolvedRemediation(
  messages: Message[],
  outcome: string | undefined,
): number {
  return messages.filter((message) =>
    shouldHideResolvedRemediation(message, outcome, true)
  ).length;
}
