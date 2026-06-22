"use client";

/**
 * SetupForm (organism) — first-run admin-password setup (design.md §3.5 Login,
 * backend contract POST /auth/setup).
 *
 * Shown ONLY when GET /api/config reports `setupRequired === true`: a fresh
 * AUTH_MODE=password instance with no env KOTOJI_AUTH_ADMIN_PASSWORD and no
 * stored hash yet. The admin chooses the single-admin password here; the backend
 * stores it hashed and immediately establishes a session.
 *
 * Validation (react-hook-form + zod, design §4.1):
 *  - min length from useConfig-independent contract constant (SetupRequest
 *    minLength: 8 in openapi.yaml) — surfaced as a localized inline message,
 *  - confirm must equal password (mismatch shown inline on the confirm field).
 *
 * Submit posts to the backend with a PLAIN fetch (not the typed apiClient): the
 * password travels in the JSON BODY (never the URL), `credentials:'include'` so
 * the backend can set the __Host- session + CSRF cookies on the 200. /auth/setup
 * is mounted OUTSIDE the /api CSRF subtree, so no CSRF token is required (and if
 * a stale CSRF cookie exists, sending it would be harmless — we simply don't).
 *
 *  - 200 → session cookie is set → onDone() (the page navigates to the validated
 *          `next`, default /dashboard),
 *  - 409 → setup already completed; onAlreadyDone() flips the page back to the
 *          normal sign-in UI (no admin reset is ever possible here),
 *  - 422 → inline validation error,
 *  - other/network → inline generic error.
 */

import { useMemo, useState } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { useTranslations } from "next-intl";
import { KeyRound } from "lucide-react";

import { FormField } from "@/components/molecules/form-field";
import { Spinner } from "@/components/atoms";
import { Form } from "@/components/ui/form";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Alert, AlertDescription } from "@/components/ui/alert";

// The backend origin (dev http://localhost:8080; prod "" = same-origin). Mirrors
// the login page so both auth paths target the same backend.
const API_BASE = process.env.NEXT_PUBLIC_API_BASE_URL ?? "";

/**
 * Minimum admin-password length. Mirrors SetupRequest.minLength (8) in
 * docs/contracts/openapi.yaml — kept as a single named constant rather than a
 * magic number so the client message matches the server's 422 boundary.
 */
const PASSWORD_MIN_LEN = 8;

export interface SetupFormProps {
  /** Called on a successful 200 (session established). The page redirects. */
  onDone: () => void;
  /**
   * Called on a 409 (an admin credential already exists). The page falls back to
   * the normal sign-in UI — the setup endpoint can never reset the admin.
   */
  onAlreadyDone: () => void;
  className?: string;
}

export function SetupForm({ onDone, onAlreadyDone, className }: SetupFormProps) {
  const t = useTranslations("auth");

  // A submit-level error (422 / network) shown in an inline Alert above the
  // actions. Field-level errors live on the form fields themselves.
  const [submitError, setSubmitError] = useState<string | null>(null);

  // Build the schema once: min-length on the password and an equality refine for
  // the confirmation, both with localized messages. The mismatch error is
  // attached to the `confirm` field so it renders under the right control.
  const schema = useMemo(
    () =>
      z
        .object({
          password: z.string().min(PASSWORD_MIN_LEN, {
            message: t("passwordTooShort", { min: PASSWORD_MIN_LEN }),
          }),
          confirm: z.string(),
        })
        .refine((v) => v.password === v.confirm, {
          message: t("passwordMismatch"),
          path: ["confirm"],
        }),
    [t],
  );

  type FormValues = z.infer<typeof schema>;

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    mode: "onChange",
    defaultValues: { password: "", confirm: "" },
  });

  const [submitting, setSubmitting] = useState(false);

  const onSubmit = form.handleSubmit(async (values) => {
    setSubmitError(null);
    setSubmitting(true);
    try {
      // Plain fetch (not apiClient): /auth/setup is outside the typed /api tree
      // and outside CSRF. The password is in the BODY; credentials:'include' lets
      // the backend set the session + CSRF cookies on the 200.
      const res = await fetch(`${API_BASE}/auth/setup`, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password: values.password }),
      });

      if (res.ok) {
        // Session cookie set by the backend → let the page navigate to `next`.
        onDone();
        return;
      }

      if (res.status === 409) {
        // Setup already completed (env or DB credential exists). Fall back to the
        // sign-in UI — this endpoint can never reset the admin.
        onAlreadyDone();
        return;
      }

      if (res.status === 422) {
        // Server rejected the password (e.g. policy). Show it inline; prefer the
        // server's already-localized message when present.
        setSubmitError(await readServerMessage(res, t("setupError")));
        return;
      }

      // Any other status: generic inline error.
      setSubmitError(await readServerMessage(res, t("setupError")));
    } catch {
      // Transport failure (server unreachable / non-JSON). Generic inline error.
      setSubmitError(t("setupError"));
    } finally {
      setSubmitting(false);
    }
  });

  return (
    <Form {...form}>
      <form
        onSubmit={onSubmit}
        className={className}
        data-slot="setup-form"
        noValidate
      >
        <div className="space-y-5">
          <div className="flex flex-col items-center gap-1 text-center">
            <span className="mb-1 inline-flex size-9 items-center justify-center rounded-full bg-accent text-accent-foreground">
              <KeyRound className="size-4" aria-hidden="true" />
            </span>
            <h1 className="text-xl font-semibold text-foreground">
              {t("setupTitle")}
            </h1>
            <p className="text-sm text-muted-foreground text-balance">
              {t("setupSubtitle")}
            </p>
          </div>

          {submitError ? (
            <Alert variant="destructive">
              <AlertDescription>{submitError}</AlertDescription>
            </Alert>
          ) : null}

          <div className="space-y-3">
            <FormField
              control={form.control}
              name="password"
              label={t("newPassword")}
              required
              description={t("passwordHint", { min: PASSWORD_MIN_LEN })}
              render={(field) => (
                <Input
                  {...field}
                  type="password"
                  autoComplete="new-password"
                />
              )}
            />

            <FormField
              control={form.control}
              name="confirm"
              label={t("confirmPassword")}
              required
              render={(field) => (
                <Input
                  {...field}
                  type="password"
                  autoComplete="new-password"
                />
              )}
            />
          </div>

          <Button
            type="submit"
            className="w-full"
            disabled={submitting || !form.formState.isValid}
            aria-busy={submitting}
          >
            {submitting ? <Spinner size="sm" /> : null}
            {t("setupSubmit")}
          </Button>
        </div>
      </form>
    </Form>
  );
}

/**
 * Best-effort extraction of the backend ErrorEnvelope message for an inline
 * error, falling back to a localized generic string. Defensive: the body may be
 * empty or non-JSON, so any parse failure yields the fallback.
 */
async function readServerMessage(res: Response, fallback: string): Promise<string> {
  try {
    const body: unknown = await res.json();
    if (
      typeof body === "object" &&
      body !== null &&
      "error" in body &&
      typeof (body as { error: unknown }).error === "object" &&
      (body as { error: { message?: unknown } }).error !== null
    ) {
      const msg = (body as { error: { message?: unknown } }).error.message;
      if (typeof msg === "string" && msg.length > 0) return msg;
    }
  } catch {
    // Non-JSON / empty body → fall through to the localized fallback.
  }
  return fallback;
}
