/**
 * Typed API error layer.
 *
 * The backend wire envelope is FROZEN in CANONICAL.md §3 and openapi.yaml:
 *
 *   { "error": { "code": "conflict", "message": "...", "details": { ... } } }
 *
 * The optimistic-lock conflict (code "conflict") carries the frozen detail
 * shape `{ branch, expected, actual, changedPaths }` (CANONICAL.md §8). We
 * surface that specially as `ConflictError` so the editor's ConflictResolver
 * (design.md §4.1 / organism ConflictResolver) can branch on it with `instanceof`
 * without re-parsing strings.
 *
 * Everything here is framework-agnostic (no React, no TanStack) so it is unit
 * testable and reusable by the openapi-fetch middleware AND by the query hooks.
 */

import type { components } from "./schema";

/** The stable machine error codes (CANONICAL.md §3, openapi.yaml ErrorEnvelope). */
export type ApiErrorCode =
  | "unauthenticated"
  | "forbidden"
  | "validation"
  | "conflict"
  | "not_found"
  | "handle_taken"
  | "publish_conflict"
  | "branch_exists"
  | "nothing_to_commit"
  | "too_large"
  | "unsupported_media_type"
  | "rate_limited"
  | "quota_exceeded"
  | "internal"
  // Fallback for transport-level failures (network down, non-JSON body, etc.)
  // where the server never produced an envelope.
  | "network";

/** The wire shape of the error envelope body (mirrors openapi ErrorEnvelope). */
export interface ErrorEnvelopeBody {
  error: {
    code: string;
    message: string;
    details?: Record<string, unknown> | null;
  };
}

/** Frozen optimistic-lock conflict detail (CANONICAL.md §8). */
export type ConflictDetail = components["schemas"]["ConflictError"];

/** Frozen publish-merge-conflict detail (openapi PublishConflictEnvelope). */
export interface PublishConflictDetail {
  paths: string[];
}

/**
 * ApiError — the single normalized error every hook/component branches on.
 *
 * TanStack Query carries this as the `error`, so UI can do:
 *   if (error instanceof ConflictError) ...      // optimistic-lock resolver
 *   if (error?.status === 401) redirect to login // handled centrally
 *   error.code === "forbidden"                    // not-authorized state
 */
export class ApiError extends Error {
  /** HTTP status (0 for transport failures that never reached the server). */
  readonly status: number;
  /** Machine code from the envelope; "network" for transport failures. */
  readonly code: ApiErrorCode;
  /** Raw structured detail (code-specific). */
  readonly details: Record<string, unknown> | null;

  constructor(
    status: number,
    code: ApiErrorCode,
    message: string,
    details: Record<string, unknown> | null = null
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.details = details;
    // Restore prototype chain so `instanceof` works after TS down-compilation.
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * ConflictError — the optimistic-lock (stale baseSha) conflict.
 *
 * `instanceof ApiError` is also true (subclass), and `code === "conflict"`.
 * The editor uses `expected`/`actual`/`changedPaths` to drive ConflictResolver.
 */
export class ConflictError extends ApiError {
  readonly branch: string;
  readonly expected: string;
  readonly actual: string;
  readonly changedPaths: string[];

  constructor(
    status: number,
    message: string,
    detail: ConflictDetail,
    rawDetails: Record<string, unknown> | null = null
  ) {
    super(status, "conflict", message, rawDetails);
    this.name = "ConflictError";
    this.branch = detail.branch;
    this.expected = detail.expected;
    this.actual = detail.actual;
    this.changedPaths = detail.changedPaths ?? [];
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * PublishConflictError — a publish-time merge conflict (published moved under
 * us). Distinct from the stale-base ConflictError so PublishPanel can explain
 * the conflicting paths.
 */
export class PublishConflictError extends ApiError {
  readonly paths: string[];

  constructor(
    status: number,
    message: string,
    paths: string[],
    rawDetails: Record<string, unknown> | null = null
  ) {
    super(status, "publish_conflict", message, rawDetails);
    this.name = "PublishConflictError";
    this.paths = paths;
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/** Narrow an unknown code string to a known ApiErrorCode, defaulting to internal. */
function normalizeCode(code: unknown): ApiErrorCode {
  const known: ApiErrorCode[] = [
    "unauthenticated",
    "forbidden",
    "validation",
    "conflict",
    "not_found",
    "handle_taken",
    "publish_conflict",
    "branch_exists",
    "nothing_to_commit",
    "too_large",
    "unsupported_media_type",
    "rate_limited",
    "quota_exceeded",
    "internal",
    "network",
  ];
  return known.includes(code as ApiErrorCode) ? (code as ApiErrorCode) : "internal";
}

/** Type guard for the wire envelope so we can parse it defensively. */
function isErrorEnvelope(body: unknown): body is ErrorEnvelopeBody {
  return (
    typeof body === "object" &&
    body !== null &&
    "error" in body &&
    typeof (body as { error: unknown }).error === "object" &&
    (body as { error: unknown }).error !== null
  );
}

function looksLikeConflictDetail(d: unknown): d is ConflictDetail {
  return (
    typeof d === "object" &&
    d !== null &&
    "expected" in d &&
    "actual" in d &&
    "branch" in d
  );
}

/**
 * parseApiError — turn an HTTP `Response` + already-parsed body into the right
 * typed error. The openapi-fetch middleware calls this on any non-2xx; the
 * specialized subclasses let the UI branch with `instanceof`.
 *
 * @param response the fetch Response (status is the authority for the HTTP code)
 * @param body the parsed JSON body (may be undefined for empty/non-JSON bodies)
 */
export function parseApiError(response: Response, body: unknown): ApiError {
  // Defensive default: a server error with no parseable envelope.
  if (!isErrorEnvelope(body)) {
    // 401/403 commonly have no body in some setups; still classify by status.
    const fallbackCode: ApiErrorCode =
      response.status === 401
        ? "unauthenticated"
        : response.status === 403
          ? "forbidden"
          : response.status === 404
            ? "not_found"
            : "internal";
    return new ApiError(
      response.status,
      fallbackCode,
      response.statusText || "リクエストに失敗しました",
      null
    );
  }

  const { code, message, details } = body.error;
  const normalized = normalizeCode(code);
  const safeDetails = (details ?? null) as Record<string, unknown> | null;

  // Optimistic-lock conflict — surface the specialized type.
  if (normalized === "conflict" && looksLikeConflictDetail(details)) {
    return new ConflictError(response.status, message, details, safeDetails);
  }

  // Publish merge conflict — specialized type with conflicting paths.
  if (
    normalized === "publish_conflict" &&
    typeof details === "object" &&
    details !== null &&
    Array.isArray((details as { paths?: unknown }).paths)
  ) {
    return new PublishConflictError(
      response.status,
      message,
      (details as { paths: string[] }).paths,
      safeDetails
    );
  }

  return new ApiError(response.status, normalized, message, safeDetails);
}

/** Build a transport-level (network) ApiError when fetch itself throws. */
export function networkError(cause?: unknown): ApiError {
  const msg =
    cause instanceof Error ? cause.message : "ネットワークに接続できませんでした";
  return new ApiError(0, "network", msg, null);
}

// --------------------------------------------------------------- helpers

/** Is this an ApiError? (works across module boundaries via instanceof). */
export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError;
}

/** Is this the optimistic-lock conflict (stale baseSha)? */
export function isConflictError(err: unknown): err is ConflictError {
  return err instanceof ConflictError;
}

/** Is this a publish merge conflict? */
export function isPublishConflictError(err: unknown): err is PublishConflictError {
  return err instanceof PublishConflictError;
}

/** Is this an auth failure the app must redirect-to-login on? */
export function isUnauthenticated(err: unknown): boolean {
  return isApiError(err) && (err.status === 401 || err.code === "unauthenticated");
}

/** Is this an authorization (403) failure? */
export function isForbidden(err: unknown): boolean {
  return isApiError(err) && (err.status === 403 || err.code === "forbidden");
}

/**
 * Best-effort human message for toasts. Prefers the server message (already
 * localized/safe per CANONICAL.md §3) and falls back to a generic JP string.
 */
export function errorMessage(err: unknown, fallback = "問題が発生しました"): string {
  if (isApiError(err)) return err.message || fallback;
  if (err instanceof Error) return err.message || fallback;
  return fallback;
}
