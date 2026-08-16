import type { Page } from "@playwright/test";

/**
 * Captures runtime errors (uncaught exceptions and console.error)
 * from a Playwright page. Attach early in the test, then query
 * categorized results after interactions complete.
 */
export class RuntimeErrorMonitor {
  private readonly errors: string[] = [];
  private readonly networkDiagnostics: string[] = [];

  constructor(page: Page) {
    page.on("pageerror", (err) => {
      this.errors.push(err.message);
    });
    page.on("console", (msg) => {
      if (msg.type() !== "error") {
        return;
      }
      const location = msg.location();
      if (
        msg.text().startsWith("Failed to load resource:") &&
        location.url &&
        !isMonitoredOrigin(location.url, page)
      ) {
        return;
      }
      const suffix = location.url
        ? ` (${formatURL(location.url, page.url())}:${location.line + 1}:${location.column + 1})`
        : "";
      this.errors.push(`${msg.text()}${suffix}`);
    });
    page.on("response", (response) => {
      if (response.status() < 400 || !isMonitoredOrigin(response.url(), page)) {
        return;
      }
      const request = response.request();
      this.networkDiagnostics.push(
        `HTTP ${response.status()} ${request.method()} ${formatURL(response.url(), page.url())} [${request.resourceType()}]`,
      );
    });
    page.on("requestfailed", (request) => {
      if (!isMonitoredOrigin(request.url(), page)) {
        return;
      }
      const errorText = request.failure()?.errorText ?? "unknown error";
      this.networkDiagnostics.push(
        `REQUEST FAILED ${request.method()} ${formatURL(request.url(), page.url())} [${request.resourceType()}]: ${errorText}`,
      );
    });
  }

  /** All captured error messages. */
  all(): readonly string[] {
    return this.errors;
  }

  /** Errors matching the given pattern. */
  matching(pattern: RegExp): string[] {
    return this.errors.filter((m) => {
      pattern.lastIndex = 0;
      return pattern.test(m);
    });
  }

  /** Errors not matching the given pattern. */
  excluding(pattern: RegExp): string[] {
    return this.errors.filter((m) => {
      pattern.lastIndex = 0;
      return !pattern.test(m);
    });
  }

  /** Failed HTTP responses and requests observed while the test ran. */
  diagnostics(): readonly string[] {
    return this.networkDiagnostics;
  }

  /** Network context suitable for an assertion failure message. */
  diagnosticSummary(): string {
    if (this.networkDiagnostics.length === 0) {
      return "Network diagnostics: none recorded";
    }
    return `Network diagnostics:\n${this.networkDiagnostics.join("\n")}`;
  }
}

function isMonitoredOrigin(rawURL: string, page: Page): boolean {
  try {
    const requestURL = new URL(rawURL);
    const pageURL = new URL(page.url());
    return requestURL.origin === pageURL.origin;
  } catch {
    return false;
  }
}

function formatURL(rawURL: string, pageURL: string): string {
  try {
    const url = new URL(rawURL);
    const current = new URL(pageURL);
    const origin = url.origin === current.origin ? "" : url.origin;
    const query = Array.from(
      url.searchParams.keys(),
      (key) => `${encodeURIComponent(key)}=[redacted]`,
    ).join("&");
    return `${origin}${url.pathname}${query ? `?${query}` : ""}`;
  } catch {
    return rawURL;
  }
}
