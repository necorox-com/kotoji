"use client";

/**
 * useRequireAuth — client-side auth gate (design.md §4.3 AuthGate).
 *
 * Reads the session via useMe() (GET /api/me). While resolving, callers show a
 * spinner. On 401 (isUnauthenticated) we redirect to /auth/login with a `next`
 * param so the backend can bounce the user back after OIDC. Optionally requires
 * instance-admin (users.isAdmin) for the Admin routes; non-admins are sent to
 * /dashboard (design.md §3.5 Admin: admin-guarded).
 *
 * NOTE: this complements (does not replace) the server-side guard the (app)
 * layout performs; it covers client navigations and session expiry mid-session.
 */

import { useEffect } from "react";
import { usePathname, useRouter } from "next/navigation";
import { useMe } from "@/lib/api/hooks";
import { isUnauthenticated } from "@/lib/api/error";

export interface UseRequireAuthOptions {
  /** Require instance-admin (users.isAdmin). Default false. */
  requireAdmin?: boolean;
  /** Where to send unauthenticated users (the backend login entry). */
  loginPath?: string;
}

export function useRequireAuth(options: UseRequireAuthOptions = {}) {
  const { requireAdmin = false, loginPath = "/auth/login" } = options;
  const router = useRouter();
  const pathname = usePathname();
  const query = useMe();
  const { data: me, error, isLoading, isError } = query;

  useEffect(() => {
    // Not authenticated -> bounce to login, preserving the intended target.
    if (isError && isUnauthenticated(error)) {
      const next = encodeURIComponent(pathname || "/dashboard");
      // Full navigation (not router.push) so we hit the backend OIDC entry.
      window.location.assign(`${loginPath}?next=${next}`);
      return;
    }
    // Authenticated but not an admin on an admin-only route -> dashboard.
    if (requireAdmin && me && !me.user.isAdmin) {
      router.replace("/dashboard");
    }
  }, [isError, error, me, requireAdmin, pathname, loginPath, router]);

  const authorized = !!me && (!requireAdmin || me.user.isAdmin);

  return {
    me,
    /** True while the session is still being determined. */
    isLoading,
    /** True once the user is present and (if required) an admin. */
    authorized,
    /** The raw query for advanced cases (refetch, etc.). */
    query,
  };
}
