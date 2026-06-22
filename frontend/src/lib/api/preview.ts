/**
 * preview-grant helper.
 *
 * The preview-grant route (POST /api/sites/{handle}/branches/{branch}/preview-grant)
 * is an ad-hoc hardening endpoint and is intentionally NOT in the generated
 * OpenAPI client. We call it with a plain fetch that mirrors the typed client's
 * conventions: same-origin credentials + the (possibly __Host- prefixed) CSRF
 * double-submit header. It returns a one-time signed preview URL (…/?kpt=<grant>)
 * the browser can open directly; the data plane validates the grant, sets a
 * host-only cookie, and serves the non-published branch.
 */
import { readCookie, CSRF_COOKIE, CSRF_HEADER } from "./client";

const API_BASE = process.env.NEXT_PUBLIC_API_BASE_URL ?? "";

export interface PreviewGrant {
  /** Full preview URL with the one-time ?kpt grant appended. Open this. */
  previewUrl: string;
  grant: string;
  branch: string;
  expiresAt: string;
}

/** Request a one-time signed preview URL for a non-published branch. */
export async function requestPreviewGrant(
  handle: string,
  branch: string,
): Promise<PreviewGrant> {
  const csrf = readCookie(`__Host-${CSRF_COOKIE}`) || readCookie(CSRF_COOKIE);
  const res = await fetch(
    `${API_BASE}/api/sites/${encodeURIComponent(handle)}/branches/${encodeURIComponent(branch)}/preview-grant`,
    {
      method: "POST",
      credentials: "include",
      headers: { "Content-Type": "application/json", [CSRF_HEADER]: csrf },
    },
  );
  if (!res.ok) {
    let message = `preview grant failed (${res.status})`;
    try {
      const body = await res.json();
      message = body?.error?.message ?? message;
    } catch {
      /* non-JSON error body — keep the status message */
    }
    throw new Error(message);
  }
  return (await res.json()) as PreviewGrant;
}
