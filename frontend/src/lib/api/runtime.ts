const SERVER_URL_KEY = "agentsview-server-url";
const AUTH_TOKEN_KEY = "agentsview-auth-token";

export function getBase(): string {
  const server = getServerUrl();
  if (server) return `${server}/api/v1`;
  const baseEl = document.querySelector("base[href]");
  if (baseEl) {
    const base = new URL(document.baseURI).pathname.replace(/\/$/, "");
    return `${base}/api/v1`;
  }
  return "/api/v1";
}

function getGeneratedBase(): string {
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

export function authHeaders(init?: RequestInit): RequestInit {
  const token = getAuthToken();
  if (!token) return init ?? {};

  const headers = new Headers(init?.headers);
  headers.set("Authorization", `Bearer ${token}`);
  return { ...init, headers };
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

function apiErrorMessage(status: number, body: string): string {
  const text = body.trim();
  if (!text) return `API ${status}`;

  try {
    const parsed = JSON.parse(text) as unknown;
    if (
      parsed !== null &&
      typeof parsed === "object" &&
      "error" in parsed &&
      typeof parsed.error === "string" &&
      parsed.error
    ) {
      return parsed.error;
    }
  } catch {
    // Plain-text error body.
  }

  return text;
}

export async function responseErrorMessage(res: Response): Promise<string> {
  const body = await res.text().catch(() => "");
  return apiErrorMessage(res.status, body);
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

export async function orvalRequest(url: string, options: RequestInit = {}): Promise<Response> {
  const response = await fetch(`${getGeneratedBase()}${url}`, {
    ...options,
    headers: generatedHeaders(options.headers),
  });
  if (response.ok) return response;

  const body = await response.text().catch(() => "");
  let error: unknown = body;
  try {
    error = JSON.parse(body);
  } catch {
    // Plain-text error body.
  }
  throw new ApiError(
    response.status,
    generatedErrorMessage(error) || `API ${response.status}`,
    generatedErrorCode(error),
  );
}

export async function orvalFetch<T>(url: string, options: RequestInit): Promise<T> {
  const response = await orvalRequest(url, options);
  if ([204, 205, 304].includes(response.status)) return undefined as T;

  const body = await response.text();
  if (!body) return undefined as T;
  if (response.headers.get("Content-Type")?.includes("json")) {
    return JSON.parse(body);
  }
  return body as T;
}

type GeneratedRequestOptions = {
  signal?: AbortSignal;
};

export async function callGenerated<T>(
  request: (options?: GeneratedRequestOptions) => Promise<T>,
  signal?: AbortSignal,
): Promise<T> {
  return request(signal ? { signal } : undefined);
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
