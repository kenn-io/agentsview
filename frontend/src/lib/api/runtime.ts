import {
  ApiError as GeneratedApiError,
  CancelError,
  OpenAPI,
} from "./generated/index";

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

export function apiErrorMessage(status: number, body: string): string {
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

export function configureGeneratedClient(): void {
  OpenAPI.BASE = getGeneratedBase();
  OpenAPI.TOKEN = async () => getAuthToken();
}

export function generatedErrorMessage(err: GeneratedApiError): string {
  if (typeof err.body === "string") {
    return apiErrorMessage(err.status, err.body);
  }
  if (
    err.body !== null &&
    typeof err.body === "object" &&
    "error" in err.body &&
    typeof err.body.error === "string" &&
    err.body.error
  ) {
    return err.body.error;
  }
  return err.message || `API ${err.status}`;
}

export function generatedErrorCode(err: GeneratedApiError): string | undefined {
  if (
    err.body !== null &&
    typeof err.body === "object" &&
    "code" in err.body &&
    typeof err.body.code === "string" &&
    err.body.code
  ) {
    return err.body.code;
  }
  return undefined;
}

export async function callGenerated<T>(
  request: () => Promise<T>,
  signal?: AbortSignal,
): Promise<T> {
  configureGeneratedClient();
  try {
    return await withAbort(request(), signal);
  } catch (err) {
    if (err instanceof GeneratedApiError) {
      throw new ApiError(
        err.status,
        generatedErrorMessage(err),
        generatedErrorCode(err),
      );
    }
    throw err;
  }
}

export interface CancelableLike<T> extends Promise<T> {
  cancel: () => void;
}

export function isCancelable<T>(value: Promise<T>): value is CancelableLike<T> {
  return typeof (value as { cancel?: unknown }).cancel === "function";
}

export function isAbortError(err: unknown): boolean {
  if (err instanceof DOMException && err.name === "AbortError") {
    return true;
  }
  if (err instanceof CancelError) {
    return true;
  }
  if (err === null || typeof err !== "object") {
    return false;
  }
  const candidate = err as {
    isCancelled?: unknown;
    name?: unknown;
  };
  return candidate.isCancelled === true ||
    candidate.name === "CancelError";
}

export function withAbort<T>(
  promise: Promise<T>,
  signal?: AbortSignal,
): Promise<T> {
  if (!signal || !isCancelable(promise)) return promise;
  if (signal.aborted) {
    promise.cancel();
    return promise;
  }
  const cancel = () => promise.cancel();
  signal.addEventListener("abort", cancel, { once: true });
  return promise.finally(() => {
    signal.removeEventListener("abort", cancel);
  });
}
