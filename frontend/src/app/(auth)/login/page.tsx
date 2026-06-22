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
 * FIRST-RUN: when the instance reports `setupRequired === true` (AUTH_MODE=
 * password AND no env admin password AND no stored hash yet — see /api/config),
 * we render the first-run admin-password setup screen INSTEAD of the sign-in UI.
 * On success the backend has set the session cookie, so we navigate client-side
 * to the validated `next`. On 409 (already set up) we fall back to sign-in.
 *
 * Auth is a full browser navigation to the Go backend (never an in-app fetch) so
 * the opaque session cookie is set by the backend; the frontend never sees tokens
 * (design.md §4.3). The card chrome (mark + tagline) is the (auth) layout.
 *
 * The `next` param (where to return after login) is read from the URL so the
 * route guard's `?next=` round-trips correctly.
 */

import { useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { useTranslations } from "next-intl";
import { LogIn } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/atoms";
import { LoadingState } from "@/components/molecules/loading-state";
import { SetupForm } from "@/components/organisms/setup-form";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useConfig } from "@/lib/api/hooks";

// The backend origin (dev http://localhost:8080; prod "" = same-origin).
const API_BASE = process.env.NEXT_PUBLIC_API_BASE_URL ?? "";

/** Build the backend login URL preserving the post-login redirect target. */
function loginUrl(next: string): string {
  const params = new URLSearchParams({ next });
  return `${API_BASE}/auth/login?${params.toString()}`;
}

/**
 * Sanitize a post-auth redirect target for CLIENT-side navigation. Only same-app
 * absolute paths are allowed — anything that could escape the origin (scheme,
 * protocol-relative "//host", or a non-"/" value) collapses to /dashboard. This
 * prevents an open-redirect via the `next` query when we use router.replace
 * (the backend validates its own `next` for the full-navigation flows).
 */
function safeNext(next: string | null): string {
  if (!next || !next.startsWith("/") || next.startsWith("//")) {
    return "/dashboard";
  }
  return next;
}

export default function LoginPage() {
  const t = useTranslations("auth");
  const router = useRouter();
  const searchParams = useSearchParams();
  const { data: config, isLoading } = useConfig();

  const [signingIn, setSigningIn] = useState(false);
  const [password, setPassword] = useState("");
  // When setup completes with a 409 (already configured), flip to the sign-in UI
  // even though a stale cached config may still report setupRequired=true.
  const [setupClosed, setSetupClosed] = useState(false);

  // Validated server-side for the full-navigation flows; for the client-side
  // setup redirect we additionally guard it against open redirects.
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

  // First-run setup screen: a password-mode instance with no credential yet.
  // `setupClosed` lets a 409 during setup immediately reveal the sign-in form.
  if (config?.setupRequired && !setupClosed) {
    return (
      <SetupForm
        onDone={() => router.replace(safeNext(searchParams.get("next")))}
        onAlreadyDone={() => setSetupClosed(true)}
      />
    );
  }

  return (
    <div className="space-y-6" data-slot="login">
      <h1 className="text-center text-xl font-semibold text-foreground">
        {t("loginTitle")}
      </h1>

      {/* A first-run setup attempt that hit a 409 (already configured) lands
          here; explain why the sign-in form is shown instead. */}
      {setupClosed ? (
        <Alert>
          <AlertDescription>{t("setupAlreadyDone")}</AlertDescription>
        </Alert>
      ) : null}

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

      {/* Admin-password mode (internal/self-host). NATIVE form POST: the password
          travels in the request BODY (never the URL/query), the backend sets the
          opaque session cookie and 302s to `next`. /auth/login is outside the
          /api CSRF subtree, so no CSRF token is needed here. */}
      {authMode === "password" ? (
        <form
          className="space-y-3"
          method="post"
          action={`${API_BASE}/auth/login`}
          onSubmit={() => setSigningIn(true)}
        >
          <input type="hidden" name="next" value={next} />
          <div className="space-y-1.5">
            <Label htmlFor="admin-password">{t("adminPassword")}</Label>
            <Input
              id="admin-password"
              name="password"
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
