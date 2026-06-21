"use client";

/**
 * AuthGate — the client-side route guard for the (app) group (design.md §4.3).
 *
 * Wraps authenticated content: while the session is resolving it shows a centered
 * spinner; on 401 useRequireAuth performs the full-navigation redirect to the
 * backend login entry (preserving `?next=`); once authorized it renders children.
 * Optionally requires instance-admin for the Admin routes (non-admins bounce to
 * /dashboard inside the hook).
 *
 * This complements (does not replace) any server-side guard — it covers client
 * navigations and mid-session expiry.
 */

import { useTranslations } from "next-intl";
import { useRequireAuth } from "@/hooks";
import { Spinner } from "@/components/atoms";

export interface AuthGateProps {
  children: React.ReactNode;
  /** Require instance-admin (users.isAdmin) — used by the Admin route. */
  requireAdmin?: boolean;
}

export function AuthGate({ children, requireAdmin }: AuthGateProps) {
  const t = useTranslations("auth");
  const { authorized, isLoading } = useRequireAuth({ requireAdmin });

  // Resolving the session, redirecting, or not-yet-authorized → spinner. We do
  // NOT render protected children until we know the user is allowed (the hook
  // drives the actual redirect on 401 / non-admin).
  if (isLoading || !authorized) {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-background">
        <Spinner size="lg" label={t("redirecting")} />
      </div>
    );
  }

  return <>{children}</>;
}
