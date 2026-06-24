"use client";

/**
 * OIDCAdminSection (organism) — the INSTANCE-level "Sign-in / 認証" config on the
 * /settings page (sibling of GitHubAdminSection / DomainAdminSection). ADMIN-ONLY:
 * the page mounts it solely when `me.user.isAdmin`, and the endpoints sit behind
 * RequireAdmin server-side (GET/PUT /api/admin/oidc).
 *
 * It makes Google sign-in (OIDC) FULLY Web-UI-configurable, removing the last
 * auth-config-by-env requirement. The flow:
 *  - GET /api/admin/oidc returns the EFFECTIVE config with WordPress-style
 *    precedence per field (env OVERRIDES DB OVERRIDES default/derived), each
 *    carrying a `*Source` ("env"|"db"|"derived") + a `*Locked` flag,
 *  - source==="env"/locked → the field is READ-ONLY with a "環境変数で設定済み /
 *    Set via environment" badge (the admin must unset the env var to edit it),
 *  - PUT persists a PARTIAL update; the runtime OIDC provider is rebuilt server-
 *    side (discovery re-runs on next sign-in).
 *
 * SECRET-SAFE: the client secret is NEVER returned — only `clientSecretSet`. The
 * secret input is WRITE-ONLY: leaving it blank KEEPS the stored value (a "設定済み
 * / Configured" badge shows when set); a non-empty value rotates it; a "クリア /
 * Clear" affordance sends `clearClientSecret:true` to revert to the env secret (or
 * none).
 *
 * FAIL-CLOSED (decision #5): enabling OIDC requires a client id + secret AND at
 * least one access gate (allowedEmails OR allowedDomains). The server rejects a
 * footgun-enable with 422; we surface the rule inline AND map the server's
 * `details.field` (clientSecret / allowedDomains / redirectUrl / a locked field)
 * back onto the offending input. A locked field returns 409 (conflict).
 *
 * The REDIRECT URL is derived from the control base URL + /auth/callback unless
 * explicitly set; we show `redirectUrlEffective` read-only (the value to register
 * in Google Cloud Console) and let the admin override it with `redirectUrl`.
 *
 * On a successful PUT the hook invalidates the public ["config"] query so
 * `authProviders` refreshes — enabling OIDC here makes the "Google で続ける" button
 * appear on the login page (no login-page change needed; it already renders one
 * control per `authProviders[]` entry).
 *
 * Validation (react-hook-form + zod) mirrors the server: issuer + redirect URL are
 * absolute http(s) URLs (both allow empty → revert to default/derived); the
 * allowlists are free CSV/textarea (the server normalizes + is the authority).
 */

import { useMemo, useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { KeyRound, Lock, CheckCircle2, TriangleAlert } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { FormField } from "@/components/molecules/form-field";
import { LoadingState } from "@/components/molecules/loading-state";
import { ErrorState } from "@/components/molecules/error-state";
import { Spinner } from "@/components/atoms";
import { Form } from "@/components/ui/form";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useAdminOIDC, useUpdateAdminOIDC } from "@/lib/api/hooks";
import { errorMessage, isApiError } from "@/lib/api/error";
import type { OIDCAdminConfig, OIDCAdminConfigUpdate } from "@/lib/api/types";

// The default Google issuer — used as the input placeholder so a fresh install
// nudges the admin toward the canonical value (the server applies it too).
const GOOGLE_ISSUER = "https://accounts.google.com";

export interface OIDCAdminSectionProps {
  className?: string;
}

// The plain (non-secret) form fields. The client secret is WRITE-ONLY and tracked
// separately so a blank value can KEEP the stored secret (never round-tripped).
interface FormValues {
  enabled: boolean;
  issuer: string;
  clientId: string;
  clientSecret: string;
  redirectUrl: string;
  // Allowlists are edited as free text (newline/comma-separated) and split on save.
  allowedEmails: string;
  allowedDomains: string;
  adminEmails: string;
}

// Server `details.field` → form field name. Used to surface a 409/422 inline on
// the offending input rather than only a toast.
type ServerField =
  | "enabled"
  | "issuer"
  | "clientId"
  | "clientSecret"
  | "redirectUrl"
  | "allowedEmails"
  | "allowedDomains"
  | "adminEmails";

export function OIDCAdminSection({ className }: OIDCAdminSectionProps) {
  const t = useTranslations("settings");
  const tc = useTranslations("common");

  const configQuery = useAdminOIDC();
  const updateOIDC = useUpdateAdminOIDC();

  // Schema mirrors the server: issuer + redirect URL are absolute http(s) URLs
  // (both allow empty → revert to default/derived). The allowlists are free text
  // (the server normalizes/validates emails + domains and stays the authority).
  const schema = useMemo(
    () =>
      z.object({
        enabled: z.boolean(),
        issuer: z
          .string()
          .trim()
          .refine((v) => v === "" || isHttpUrl(v), {
            message: t("oidcIssuerInvalid"),
          }),
        clientId: z.string(),
        clientSecret: z.string(),
        redirectUrl: z
          .string()
          .trim()
          .refine((v) => v === "" || isHttpUrl(v), {
            message: t("oidcRedirectUrlInvalid"),
          }),
        allowedEmails: z.string(),
        allowedDomains: z.string(),
        adminEmails: z.string(),
      }),
    [t],
  );

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    mode: "onChange",
    defaultValues: {
      enabled: false,
      issuer: "",
      clientId: "",
      clientSecret: "",
      redirectUrl: "",
      allowedEmails: "",
      allowedDomains: "",
      adminEmails: "",
    },
  });

  // Watch the enable switch + allowlists to drive the fail-closed inline notice
  // (enabling needs credentials + a gate). `useWatch` (the subscription hook) is
  // React-Compiler-safe vs. the non-memoizable `form.watch()` (codebase idiom).
  const enabledValue = useWatch({ control: form.control, name: "enabled" });
  const emailsValue = useWatch({ control: form.control, name: "allowedEmails" });
  const domainsValue = useWatch({
    control: form.control,
    name: "allowedDomains",
  });
  const clientIdValue = useWatch({ control: form.control, name: "clientId" });
  const secretValue = useWatch({ control: form.control, name: "clientSecret" });

  // Seed the form from the fetched config DURING render (codebase idiom; React-
  // recommended over set-state-in-effect). We seed every NON-secret field with the
  // effective value verbatim; the client secret stays blank (write-only, never
  // returned). Re-seed whenever the snapshot changes. The key folds the secret-set
  // + per-field provenance so a save (which flips those) re-seeds cleanly.
  const config = configQuery.data;
  const [seededKey, setSeededKey] = useState<string>("");
  if (config) {
    const key = oidcSeedKey(config);
    if (key !== seededKey) {
      setSeededKey(key);
      form.reset({
        enabled: config.enabled,
        issuer: config.issuer,
        clientId: config.clientId,
        clientSecret: "",
        // Show the explicitly-configured redirect (empty when derived) so saving a
        // pristine form doesn't accidentally pin the derived value.
        redirectUrl: config.redirectUrl,
        allowedEmails: config.allowedEmails.join("\n"),
        allowedDomains: config.allowedDomains.join("\n"),
        adminEmails: config.adminEmails.join("\n"),
      });
    }
  }

  if (configQuery.isLoading) {
    return <LoadingState rows={6} label={t("signin")} />;
  }
  if (configQuery.isError || !config) {
    return (
      <ErrorState
        error={configQuery.error}
        title={t("signin")}
        onRetry={() => configQuery.refetch()}
      />
    );
  }

  // The enable toggle is read-only when KOTOJI_AUTH_MODE pins the provider set.
  const enabledLocked = config.enabledLocked || config.authModeLocked;

  // Would-be effective state for the inline fail-closed notice: credentials are
  // present if a client id is configured-or-typed AND a secret is set-or-typed; a
  // gate is present if either allowlist has a non-empty entry.
  const willEnable = enabledValue && !enabledLocked;
  const hasClientId = !!clientIdValue?.trim() || config.clientIdSet;
  const hasSecret = !!secretValue?.trim() || config.clientSecretSet;
  const hasGate =
    splitList(emailsValue).length > 0 || splitList(domainsValue).length > 0;
  const missingCredsOrGate = willEnable && (!hasClientId || !hasSecret || !hasGate);

  const onSubmit = form.handleSubmit(async (values) => {
    // Build a PARTIAL update: send a plain field ONLY when it's editable (not
    // env-locked) AND differs from the seeded effective value — this avoids a
    // no-op write and avoids tripping the server's locked-field 409 for a field
    // the admin never touched. The allowlists are normalized to arrays.
    const body: OIDCAdminConfigUpdate = {};

    if (!enabledLocked && values.enabled !== config.enabled) {
      body.enabled = values.enabled;
    }
    if (!config.issuerLocked && values.issuer.trim() !== config.issuer) {
      body.issuer = values.issuer.trim();
    }
    if (!config.clientIdLocked && values.clientId.trim() !== config.clientId) {
      body.clientId = values.clientId.trim();
    }
    // Secret is write-only: send ONLY when the admin actually typed one (blank
    // KEEPS the stored value). The explicit Clear button handles removal.
    if (!config.clientSecretLocked && values.clientSecret.trim().length > 0) {
      body.clientSecret = values.clientSecret.trim();
    }
    if (
      !config.redirectUrlLocked &&
      values.redirectUrl.trim() !== config.redirectUrl
    ) {
      body.redirectUrl = values.redirectUrl.trim();
    }
    // Allowlists replace the stored list; send when the normalized set differs.
    const nextEmails = splitList(values.allowedEmails);
    if (
      !config.allowedEmailsLocked &&
      !sameList(nextEmails, config.allowedEmails)
    ) {
      body.allowedEmails = nextEmails;
    }
    const nextDomains = splitList(values.allowedDomains);
    if (
      !config.allowedDomainsLocked &&
      !sameList(nextDomains, config.allowedDomains)
    ) {
      body.allowedDomains = nextDomains;
    }
    const nextAdmins = splitList(values.adminEmails);
    if (!config.adminEmailsLocked && !sameList(nextAdmins, config.adminEmails)) {
      body.adminEmails = nextAdmins;
    }

    // Nothing changed — short-circuit (no toast spam, no write).
    if (Object.keys(body).length === 0) return;

    try {
      await updateOIDC.mutateAsync(body);
      toast.success(t("oidcSaved"));
      // Clear the write-only secret field after a successful save (the re-seed
      // keeps it blank anyway, but this avoids it lingering in the DOM).
      form.setValue("clientSecret", "");
    } catch (err) {
      handleServerError(err);
    }
  });

  // Explicitly clear the stored DB client secret (reverts to the env secret, if
  // any). Disabling OIDC first is recommended, but the server stays the authority.
  const clearSecret = async () => {
    try {
      await updateOIDC.mutateAsync({ clearClientSecret: true });
      toast.success(t("oidcSecretCleared"));
      form.setValue("clientSecret", "");
    } catch (err) {
      handleServerError(err);
    }
  };

  // Map a server field error (409 locked / 422 invalid) back onto the input
  // inline; both carry `details.field`. Fall back to a toast otherwise.
  function handleServerError(err: unknown) {
    if (isApiError(err)) {
      const field = fieldFromDetails(err.details);
      if (field) {
        form.setError(field, {
          message:
            err.code === "conflict"
              ? t("oidcLockedError")
              : err.message || t("oidcSaveError"),
        });
        return;
      }
    }
    toast.error(errorMessage(err, t("oidcSaveError")));
  }

  const busy = updateOIDC.isPending;

  return (
    <Card className={className} aria-labelledby="settings-signin">
      <CardHeader>
        <CardTitle
          id="settings-signin"
          className="flex items-center gap-2 text-lg"
        >
          <KeyRound className="size-5 shrink-0" aria-hidden="true" />
          {t("signin")}
        </CardTitle>
        <CardDescription>{t("signinDescription")}</CardDescription>
      </CardHeader>

      <CardContent>
        {/* Console caveat: a Google OAuth client must still be created in Google
            Cloud Console; register the redirect URI shown below. Everything else
            is configurable here. */}
        <Alert className="mb-6">
          <TriangleAlert className="size-4" aria-hidden="true" />
          <AlertTitle>{t("oidcConsoleNoteTitle")}</AlertTitle>
          <AlertDescription>{t("oidcConsoleNote")}</AlertDescription>
        </Alert>

        <Form {...form}>
          <form onSubmit={onSubmit} className="space-y-6" noValidate>
            {/* Enable Google sign-in — switch + label row. Locked when
                KOTOJI_AUTH_MODE pins the provider set. */}
            <FormField
              control={form.control}
              name="enabled"
              render={(field) => (
                <div className="flex items-center justify-between gap-4 rounded-lg border border-border bg-muted/30 px-3 py-2.5">
                  <div className="min-w-0 space-y-0.5">
                    <span className="flex items-center gap-2 text-sm font-medium text-foreground">
                      {t("oidcEnable")}
                      {enabledLocked ? (
                        <LockedBadge
                          tooltip={
                            config.authModeLocked
                              ? t("oidcLockedTooltipAuthMode")
                              : t("oidcLockedTooltipEnabled")
                          }
                          label={t("oidcLockedBadge")}
                        />
                      ) : null}
                    </span>
                    <p className="text-sm text-muted-foreground">
                      {t("oidcEnableHint")}
                    </p>
                  </div>
                  <Switch
                    checked={!!field.value}
                    onCheckedChange={(checked) => field.onChange(checked)}
                    aria-label={t("oidcEnable")}
                    disabled={busy || enabledLocked}
                  />
                </div>
              )}
            />

            {/* Issuer (default https://accounts.google.com). */}
            <FormField
              control={form.control}
              name="issuer"
              label={
                <span className="flex items-center gap-2">
                  {t("oidcIssuer")}
                  {config.issuerLocked ? (
                    <LockedBadge
                      tooltip={t("oidcLockedTooltipIssuer")}
                      label={t("oidcLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("oidcIssuerHint")}
              render={(field) => (
                <Input
                  {...field}
                  value={field.value ?? ""}
                  placeholder={GOOGLE_ISSUER}
                  className="font-mono sm:max-w-md"
                  autoComplete="off"
                  spellCheck={false}
                  inputMode="url"
                  disabled={busy || config.issuerLocked}
                  readOnly={config.issuerLocked}
                  aria-readonly={config.issuerLocked}
                />
              )}
            />

            {/* Client ID. */}
            <FormField
              control={form.control}
              name="clientId"
              label={
                <span className="flex items-center gap-2">
                  {t("oidcClientId")}
                  {config.clientIdLocked ? (
                    <LockedBadge
                      tooltip={t("oidcLockedTooltipClientId")}
                      label={t("oidcLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("oidcClientIdHint")}
              render={(field) => (
                <Input
                  {...field}
                  value={field.value ?? ""}
                  placeholder="123456789-abc.apps.googleusercontent.com"
                  className="font-mono sm:max-w-md"
                  autoComplete="off"
                  spellCheck={false}
                  disabled={busy || config.clientIdLocked}
                  readOnly={config.clientIdLocked}
                  aria-readonly={config.clientIdLocked}
                />
              )}
            />

            {/* Client Secret — write-only. */}
            <FormField
              control={form.control}
              name="clientSecret"
              label={
                <span className="flex items-center gap-2">
                  {t("oidcClientSecret")}
                  {config.clientSecretLocked ? (
                    <LockedBadge
                      tooltip={t("oidcLockedTooltipClientSecret")}
                      label={t("oidcLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("oidcClientSecretHint")}
              render={(field) => (
                <div className="space-y-2">
                  {config.clientSecretSet ? (
                    <p className="flex items-center gap-1.5 text-sm text-success">
                      <CheckCircle2 className="size-4" aria-hidden="true" />
                      {t("oidcConfigured")}
                    </p>
                  ) : null}
                  <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
                    <Input
                      {...field}
                      type="password"
                      value={field.value ?? ""}
                      placeholder={
                        config.clientSecretLocked
                          ? "••••••••"
                          : config.clientSecretSet
                            ? t("oidcSecretUpdatePlaceholder")
                            : "GOCSPX-…"
                      }
                      className="font-mono sm:max-w-md"
                      autoComplete="off"
                      spellCheck={false}
                      disabled={busy || config.clientSecretLocked}
                      readOnly={config.clientSecretLocked}
                      aria-readonly={config.clientSecretLocked}
                    />
                    {/* Clear only makes sense for a DB-stored secret (env-set is
                        locked; nothing to clear when none is stored). */}
                    {config.clientSecretSet && !config.clientSecretLocked ? (
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => void clearSecret()}
                        disabled={busy}
                        className="shrink-0"
                      >
                        {t("oidcSecretClear")}
                      </Button>
                    ) : null}
                  </div>
                </div>
              )}
            />

            {/* Redirect URL — auto-derived from the control base URL unless set.
                We always surface the EFFECTIVE value (the URI to register in
                Google Cloud Console) read-only, plus an optional override input. */}
            <div className="space-y-2">
              <span className="text-sm font-medium text-foreground">
                {t("oidcRedirectUrl")}
              </span>
              <div className="flex items-center gap-2 rounded-md border border-border bg-muted/40 px-3 py-2">
                <code className="min-w-0 flex-1 break-all font-mono text-xs text-muted-foreground">
                  {config.redirectUrlEffective || t("oidcRedirectUrlNone")}
                </code>
                <CopyEffective value={config.redirectUrlEffective} t={tc} />
              </div>
              <p className="text-sm text-muted-foreground">
                {config.redirectUrl
                  ? t("oidcRedirectUrlHintConfigured")
                  : t("oidcRedirectUrlHintDerived")}
              </p>
            </div>

            {/* Optional explicit redirect override. */}
            <FormField
              control={form.control}
              name="redirectUrl"
              label={
                <span className="flex items-center gap-2">
                  {t("oidcRedirectUrlOverride")}
                  {config.redirectUrlLocked ? (
                    <LockedBadge
                      tooltip={t("oidcLockedTooltipRedirectUrl")}
                      label={t("oidcLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("oidcRedirectUrlOverrideHint")}
              render={(field) => (
                <Input
                  {...field}
                  value={field.value ?? ""}
                  placeholder={config.redirectUrlEffective}
                  className="font-mono sm:max-w-md"
                  autoComplete="off"
                  spellCheck={false}
                  inputMode="url"
                  disabled={busy || config.redirectUrlLocked}
                  readOnly={config.redirectUrlLocked}
                  aria-readonly={config.redirectUrlLocked}
                />
              )}
            />

            {/* Allowed emails — CSV/newline list (fail-closed gate). */}
            <FormField
              control={form.control}
              name="allowedEmails"
              label={
                <span className="flex items-center gap-2">
                  {t("oidcAllowedEmails")}
                  {config.allowedEmailsLocked ? (
                    <LockedBadge
                      tooltip={t("oidcLockedTooltipAllowedEmails")}
                      label={t("oidcLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("oidcAllowedEmailsHint")}
              render={(field) => (
                <Textarea
                  {...field}
                  value={field.value ?? ""}
                  placeholder={"alice@example.com\nbob@example.com"}
                  rows={3}
                  className="font-mono sm:max-w-md"
                  autoComplete="off"
                  spellCheck={false}
                  disabled={busy || config.allowedEmailsLocked}
                  readOnly={config.allowedEmailsLocked}
                  aria-readonly={config.allowedEmailsLocked}
                />
              )}
            />

            {/* Allowed domains — CSV/newline list (fail-closed gate). */}
            <FormField
              control={form.control}
              name="allowedDomains"
              label={
                <span className="flex items-center gap-2">
                  {t("oidcAllowedDomains")}
                  {config.allowedDomainsLocked ? (
                    <LockedBadge
                      tooltip={t("oidcLockedTooltipAllowedDomains")}
                      label={t("oidcLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("oidcAllowedDomainsHint")}
              render={(field) => (
                <Textarea
                  {...field}
                  value={field.value ?? ""}
                  placeholder={"example.com\nnecorox.com"}
                  rows={3}
                  className="font-mono sm:max-w-md"
                  autoComplete="off"
                  spellCheck={false}
                  disabled={busy || config.allowedDomainsLocked}
                  readOnly={config.allowedDomainsLocked}
                  aria-readonly={config.allowedDomainsLocked}
                />
              )}
            />

            {/* Admin emails — CSV/newline list (auto-promotes to is_admin). */}
            <FormField
              control={form.control}
              name="adminEmails"
              label={
                <span className="flex items-center gap-2">
                  {t("oidcAdminEmails")}
                  {config.adminEmailsLocked ? (
                    <LockedBadge
                      tooltip={t("oidcLockedTooltipAdminEmails")}
                      label={t("oidcLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("oidcAdminEmailsHint")}
              render={(field) => (
                <Textarea
                  {...field}
                  value={field.value ?? ""}
                  placeholder={"admin@example.com"}
                  rows={2}
                  className="font-mono sm:max-w-md"
                  autoComplete="off"
                  spellCheck={false}
                  disabled={busy || config.adminEmailsLocked}
                  readOnly={config.adminEmailsLocked}
                  aria-readonly={config.adminEmailsLocked}
                />
              )}
            />

            {/* FAIL-CLOSED rule, always visible so the admin understands the
                constraint; escalated to a warning when enabling without it. */}
            <Alert variant={missingCredsOrGate ? "destructive" : "default"}>
              <AlertDescription>{t("oidcFailClosedRule")}</AlertDescription>
            </Alert>

            {/* Break-glass reassurance: enabling OIDC never removes the admin
                password fallback (decision #2). */}
            <p className="text-sm text-muted-foreground">
              {t("oidcBreakGlassNote")}
            </p>

            <div className="flex justify-end">
              <Button
                type="submit"
                disabled={
                  busy || !form.formState.isValid || !form.formState.isDirty
                }
                aria-busy={busy}
              >
                {busy ? <Spinner size="sm" /> : null}
                {busy ? tc("saving") : tc("save")}
              </Button>
            </div>
          </form>
        </Form>
      </CardContent>
    </Card>
  );
}

/**
 * LockedBadge — the "set via environment / locked" affordance next to a field
 * whose KOTOJI_* env var is set. The tooltip names the controlling variable.
 * (Same affordance as DomainAdminSection — kept local to avoid coupling.)
 */
function LockedBadge({ label, tooltip }: { label: string; tooltip: string }) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Badge variant="secondary" className="gap-1">
            <Lock className="size-3" aria-hidden="true" />
            {label}
          </Badge>
        }
      />
      <TooltipContent>{tooltip}</TooltipContent>
    </Tooltip>
  );
}

/**
 * CopyEffective — a small "copy" button for the effective redirect URI, so the
 * admin can paste it into Google Cloud Console. No-op (hidden) when empty.
 */
function CopyEffective({
  value,
  t,
}: {
  value: string;
  t: ReturnType<typeof useTranslations>;
}) {
  if (!value) return null;
  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      className="shrink-0"
      onClick={() => {
        void navigator.clipboard
          ?.writeText(value)
          .then(() => toast.success(t("copied")))
          .catch(() => toast.error(t("copyFailed")));
      }}
    >
      {t("copy")}
    </Button>
  );
}

/**
 * isHttpUrl — true when `v` parses as an absolute http(s) URL. Lenient (the
 * server is the authority); just blocks obvious mistakes (scheme/typos) before
 * the round-trip. Used for the issuer + redirect override.
 */
function isHttpUrl(v: string): boolean {
  let u: URL;
  try {
    u = new URL(v);
  } catch {
    return false;
  }
  return (u.protocol === "http:" || u.protocol === "https:") && !!u.hostname;
}

/**
 * splitList — normalize a free-text list (newline OR comma separated) into a
 * trimmed, de-duplicated, non-empty array. Mirrors the server's CSV intent (the
 * server lowercases/validates emails + domains); we just shape the wire array.
 */
function splitList(raw: string | undefined): string[] {
  if (!raw) return [];
  const seen = new Set<string>();
  const out: string[] = [];
  for (const part of raw.split(/[\n,]/)) {
    const v = part.trim();
    if (v.length === 0 || seen.has(v.toLowerCase())) continue;
    seen.add(v.toLowerCase());
    out.push(v);
  }
  return out;
}

/**
 * sameList — order-insensitive, case-insensitive set equality, so an edit that
 * only reorders/recases the allowlist doesn't fire a needless write. The server
 * normalizes case anyway, so the seeded effective value is already lowercased.
 */
function sameList(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  const sa = new Set(a.map((v) => v.toLowerCase()));
  for (const v of b) {
    if (!sa.has(v.toLowerCase())) return false;
  }
  return true;
}

/**
 * fieldFromDetails — pull a known form field name out of a server error's
 * `details` ({ field, reason }). Returns undefined for an unrecognized field so
 * the caller falls back to a toast. (`enabled` maps to no input — the toggle is
 * unlabeled — so we surface it via toast.)
 */
function fieldFromDetails(
  details: Record<string, unknown> | null,
): Exclude<ServerField, "enabled"> | undefined {
  const field = details?.field;
  switch (field) {
    case "issuer":
    case "clientId":
    case "clientSecret":
    case "redirectUrl":
    case "allowedEmails":
    case "allowedDomains":
    case "adminEmails":
      return field;
    default:
      return undefined;
  }
}

/**
 * oidcSeedKey — a stable fingerprint of the non-secret config + the secret-set +
 * per-field provenance, so the form re-seeds exactly when the authoritative
 * snapshot changes (e.g. after a save flips clientSecretSet or a source).
 */
function oidcSeedKey(c: OIDCAdminConfig): string {
  return [
    c.enabled,
    c.issuer,
    c.clientId,
    c.clientSecretSet,
    c.redirectUrl,
    c.allowedEmails.join(","),
    c.allowedDomains.join(","),
    c.adminEmails.join(","),
    c.enabledSource,
    c.issuerSource,
    c.clientIdSource,
    c.clientSecretSource,
    c.redirectUrlSource,
    c.allowedEmailsSource,
    c.allowedDomainsSource,
    c.adminEmailsSource,
    c.authModeLocked,
  ].join("|");
}
