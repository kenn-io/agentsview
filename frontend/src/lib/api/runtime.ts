const SERVER_URL_KEY = "agentsview-server-url";
const AUTH_TOKEN_KEY = "agentsview-auth-token";

export function getGeneratedBase(): string {
  const server = getServerUrl();
  if (server) return server;
  const baseEl = document.querySelector("base[href]");
  if (baseEl) {
    return new URL(document.baseURI).pathname.replace(/\/$/, "");
  }
  return "";
}

export function getServerUrl(): string {
  return localStorage.getItem(SERVER_URL_KEY) ?? "";
}

export function setServerUrl(url: string): void {
  if (url) {
    localStorage.setItem(SERVER_URL_KEY, url);
  } else {
    localStorage.removeItem(SERVER_URL_KEY);
  }
}

function authTokenKey(): string {
  const server = getServerUrl();
  return server ? `${AUTH_TOKEN_KEY}::${server}` : AUTH_TOKEN_KEY;
}

export function getAuthToken(): string {
  return localStorage.getItem(authTokenKey()) ?? "";
}

export function setAuthToken(token: string): void {
  const key = authTokenKey();
  if (token) {
    localStorage.setItem(key, token);
  } else {
    localStorage.removeItem(key);
  }
}

export function isRemoteConnection(): boolean {
  return getServerUrl() !== "";
}

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
    public readonly code?: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

function generatedHeaders(init?: HeadersInit): Headers {
  const headers = new Headers(init);
  const token = getAuthToken();
  if (token && !headers.has("Authorization")) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  return headers;
}

export function generatedErrorMessage(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (typeof err === "string") {
    return err.trim() || "API error";
  }
  if (
    err !== null &&
    typeof err === "object" &&
    "error" in err &&
    typeof err.error === "string" &&
    err.error
  ) {
    return err.error;
  }
  return err instanceof Error ? err.message : "API error";
}

function generatedErrorCode(err: unknown): string | undefined {
  if (
    err !== null &&
    typeof err === "object" &&
    "code" in err &&
    typeof err.code === "string" &&
    err.code
  ) {
    return err.code;
  }
  return undefined;
}

export type ApiRequestOptions = RequestInit & {
  baseUrl?: string;
};

export async function orvalRequest<T extends Response = Response>(
  url: string,
  options: ApiRequestOptions = {},
): Promise<T> {
  const { baseUrl, ...init } = options;
  const response = await fetch(`${baseUrl ?? getGeneratedBase()}${url}`, {
    ...init,
    headers: baseUrl === undefined ? generatedHeaders(init.headers) : init.headers,
  });
  if (response.ok) return response as T;

  const body = await response.text().catch(() => "");
  let error: unknown = body;
  try {
    error = JSON.parse(body);
  } catch {
    // Plain-text error body.
  }
  throw new ApiError(
    response.status,
    body.trim() ? generatedErrorMessage(error) : `API ${response.status}`,
    generatedErrorCode(error),
  );
}

/**
 * When a generated request's phases happened, on the `performance.now()`
 * clock: request sent, response headers received (the server's share), and
 * response body read. Pages use it to draw request timelines.
 */
export interface ResponseTiming {
  sentAt: number;
  headersAt: number;
  bodyAt: number;
}

// Keyed by the parsed response object itself, so callers that only see the
// data can still look up how the request that produced it went.
const responseTimings = new WeakMap<object, ResponseTiming>();

export function attachResponseTiming(data: unknown, timing: ResponseTiming): void {
  if (typeof data === "object" && data !== null) responseTimings.set(data, timing);
}

export function responseTimingOf(data: unknown): ResponseTiming | undefined {
  return typeof data === "object" && data !== null ? responseTimings.get(data) : undefined;
}

export async function orvalFetch<T>(url: string, options: ApiRequestOptions): Promise<T> {
  const sentAt = performance.now();
  const response = await orvalRequest(url, options);
  const headersAt = performance.now();
  if ([204, 205, 304].includes(response.status)) return undefined as T;

  const body = await response.text();
  const bodyAt = performance.now();
  if (!body) return undefined as T;
  if (response.headers.get("Content-Type")?.includes("json")) {
    const data: T = JSON.parse(body);
    attachResponseTiming(data, { sentAt, headersAt, bodyAt });
    return data;
  }
  return body as T;
}

export function isNotFoundError(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404;
}

export function isAbortError(err: unknown): boolean {
  if (err instanceof DOMException && err.name === "AbortError") {
    return true;
  }
  if (err === null || typeof err !== "object") {
    return false;
  }
  const candidate = err as {
    name?: unknown;
  };
  return candidate.name === "AbortError";
}
