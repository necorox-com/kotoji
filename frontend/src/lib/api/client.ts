/**
 * Typed backend API client (openapi-fetch), the single source of truth for
 * every control-plane call. Generated `paths` from ./schema.d.ts make calls
 * compile-time-safe (design.md §4.1, CANONICAL.md decision #1).
 *
 * Responsibilities wired here:
 *  - baseUrl: NEXT_PUBLIC_API_BASE_URL (dev http://localhost:8080; prod "" =
 *    same-origin behind the reverse proxy).
 *  - credentials:"include": send the __Host- session cookie on every request.
 *  - CSRF: double-submit. On mutating methods we read the `kotoji_csrf` cookie
 *    and echo it as the `X-CSRF-Token` header (the server compares the two).
 *  - Errors: openapi-fetch does NOT throw on non-2xx; it returns
 *    `{ data, error, response }`. Hooks call `unwrap()` to convert that into a
 *    typed ApiError (throwing) so TanStack Query carries it as `error`.
 */

import createClient, { type Middleware } from "openapi-fetch";
import type { paths } from "./schema";
import { networkError, parseApiError, type ApiError } from "./error";

// Empty base URL => same-origin; the browser hits the proxy at the current host.
const baseUrl = process.env.NEXT_PUBLIC_API_BASE_URL ?? "";

/** Name of the double-submit CSRF cookie the backend sets (readable by JS). */
export const CSRF_COOKIE = "kotoji_csrf";
/** Header the backend expects the cookie value echoed in (double-submit). */
export const CSRF_HEADER = "X-CSRF-Token";

/** HTTP methods that mutate state and therefore require the CSRF header. */
const MUTATING_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

/**
 * Read a cookie by name from document.cookie. SSR-safe: returns "" when there
 * is no document (server render) — mutations only ever fire client-side, so the
 * CSRF token is always available where it matters.
 */
export function readCookie(name: string): string {
  if (typeof document === "undefined") return "";
  // Split on "; " and find the matching name=value pair; decode the value.
  const prefix = `${name}=`;
  for (const part of document.cookie.split("; ")) {
    if (part.startsWith(prefix)) {
      return decodeURIComponent(part.slice(prefix.length));
    }
  }
  return "";
}

/**
 * CSRF middleware: inject X-CSRF-Token on mutating requests (double-submit).
 * Reads `kotoji_csrf` fresh on each request so a rotated token is always used.
 */
const csrfMiddleware: Middleware = {
  onRequest({ request }) {
    if (MUTATING_METHODS.has(request.method.toUpperCase())) {
      // In production (Secure cookies) the backend names the cookie with the
      // `__Host-` prefix; in insecure dev it uses the bare name. Try both so the
      // double-submit header is always set.
      const token =
        readCookie(`__Host-${CSRF_COOKIE}`) || readCookie(CSRF_COOKIE);
      if (token) {
        request.headers.set(CSRF_HEADER, token);
      }
    }
    return request;
  },
};

export const apiClient = createClient<paths>({
  baseUrl,
  credentials: "include",
});

apiClient.use(csrfMiddleware);

export type ApiClient = typeof apiClient;

/**
 * The shape openapi-fetch returns from every call. `data` on success, `error`
 * (the parsed error envelope body) on failure, plus the raw `response`.
 */
export interface FetchOutcome<TData> {
  data?: TData;
  error?: unknown;
  response: Response;
}

/**
 * unwrap — turn an openapi-fetch result into the success payload or THROW a
 * typed ApiError. This is the seam that lets every TanStack hook simply
 * `return unwrap(await apiClient.GET(...))` and get conflict/auth branching for
 * free (the error reaches `useQuery().error` / `useMutation().error`).
 */
export function unwrap<TData>(result: FetchOutcome<TData>): TData {
  const { data, response } = result;
  if (response.ok) {
    // 204 No Content yields undefined data; callers that expect void are fine.
    return data as TData;
  }
  // Non-2xx: openapi-fetch already parsed the JSON body into `error`.
  throw parseApiError(response, result.error);
}

/**
 * call — wrap an openapi-fetch invocation so transport failures (fetch throws,
 * e.g. network down) also become typed ApiErrors instead of raw TypeErrors.
 * Hooks use this for resilience; `unwrap` alone is fine when offline handling
 * is not a concern.
 */
export async function call<TData>(
  fn: () => Promise<FetchOutcome<TData>>
): Promise<TData> {
  let result: FetchOutcome<TData>;
  try {
    result = await fn();
  } catch (cause) {
    // fetch itself rejected (DNS, offline, CORS) — never produced a Response.
    throw networkError(cause);
  }
  return unwrap(result);
}

export type { ApiError };
