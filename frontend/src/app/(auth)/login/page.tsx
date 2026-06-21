"use client";

/**
 * Login page (design.md §3.5 Login).
 *
 * Reads the enabled auth mode from the PUBLIC instance config (useConfig →
 * authMode: oidc | password | none) and shows ONLY the available providers:
 *  - oidc      → "Google で続ける" (full navigation to the backend /auth/login,
 *                which 302s to the IdP; carries the validated `next` target),
 *  - password  → an admin-password field posting to the backend login,
 *  - none/dev  → an explicit, clearly-labeled "開発モードで入る" button.
 *
 * Auth is a full browser navigation to the Go backend (never an in-app fetch) so
 * the opaque session cookie is set by the backend; the frontend never sees tokens
 * (design.md §4.3). The card chrome (mark + tagline) is the (auth) layout.
 *
 * The `next` param (where to return after login) is read from the URL so the
 * route guard's `?next=` round-trips correctly.
 */

import { useState } from "react";
import { useSearchParams } from "next/navigation";
import { useTranslations } from "next-intl";
import { LogIn } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/atoms";
import { LoadingState } from "@/components/molecules/loading-state";
import { useConfig } from "@/lib/api/hooks";

// The backend origin (dev http://localhost:8080; prod "" = same-origin).
const API_BASE = process.env.NEXT_PUBLIC_API_BASE_URL ?? "";

/** Build the backend login URL preserving the post-login redirect target. */
function loginUrl(next: string): string {
  const params = new URLSearchParams({ next });
  return `${API_BASE}/auth/login?${params.toString()}`;
}

export default function LoginPage() {
  const t = useTranslations("auth");
  const searchParams = useSearchParams();
  const { data: config, isLoading } = useConfig();

  const [signingIn, setSigningIn] = useState(false);
  const [password, setPassword] = useState("");

  // Validated server-side; default to the dashboard.
  const next = searchParams.get("next") || "/dashboard";

  // Full navigation to the backend (sets the session cookie, then 302s back).
  const go = (url: string) => {
    setSigningIn(true);
    window.location.assign(url);
  };

  if (isLoading) {
    return <LoadingState label={t("redirecting")} />;
  }

  const authMode = config?.authMode ?? "oidc";

  return (
    <div className="space-y-6" data-slot="login">
      <h1 className="text-center text-xl font-semibold text-foreground">
        {t("loginTitle")}
      </h1>

      {/* OIDC (Google) — the primary path. */}
      {authMode === "oidc" ? (
        <Button
          className="w-full"
          onClick={() => go(loginUrl(next))}
          disabled={signingIn}
          aria-busy={signingIn}
        >
          {signingIn ? <Spinner size="sm" /> : <LogIn aria-hidden="true" />}
          {t("continueWithGoogle")}
        </Button>
      ) : null}

      {/* Admin-password mode — internal/self-host. Posts to the same backend
          entry; the field is sent as the `password` query so the backend can
          establish the session, then redirect to `next`. */}
      {authMode === "password" ? (
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            const params = new URLSearchParams({ next, password });
            go(`${API_BASE}/auth/login?${params.toString()}`);
          }}
        >
          <div className="space-y-1.5">
            <Label htmlFor="admin-password">{t("adminPassword")}</Label>
            <Input
              id="admin-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
          </div>
          <Button
            type="submit"
            className="w-full"
            disabled={signingIn || password.length === 0}
            aria-busy={signingIn}
          >
            {signingIn ? <Spinner size="sm" /> : null}
            {t("loginTitle")}
          </Button>
          <p className="text-center text-xs text-muted-foreground">
            {t("internalUse")}
          </p>
        </form>
      ) : null}

      {/* Dev / no-auth mode — an explicit, clearly-labeled entry. */}
      {authMode === "none" ? (
        <Button
          variant="outline"
          className="w-full"
          onClick={() => go(loginUrl(next))}
          disabled={signingIn}
          aria-busy={signingIn}
        >
          {signingIn ? <Spinner size="sm" /> : <LogIn aria-hidden="true" />}
          {t("devMode")}
        </Button>
      ) : null}
    </div>
  );
}
