"use client";

/**
 * GitHubAdminSection (organism) — the INSTANCE-level GitHub mirror config on the
 * /settings page (distinct from the per-site GitHubSection, which only links a
 * repo). ADMIN-ONLY: the page mounts this solely when `me.user.isAdmin`, and the
 * underlying endpoints sit behind RequireAdmin server-side.
 *
 * It reads the EFFECTIVE config via GET /api/admin/github (DB overrides env) and
 * persists a PARTIAL update via PUT /api/admin/github. The config is SECRET-SAFE:
 *  - the PAT and webhook secret are NEVER returned — only "configured" booleans
 *    (`tokenSet` / `webhookSecretSet`),
 *  - both are WRITE-ONLY: leaving a field blank KEEPS the stored value, so the
 *    inputs render empty with a "設定済み/Configured" hint when already set,
 *  - the token additionally supports an explicit clear (clearToken:true) to
 *    revert to the env token (or none).
 *
 * On a successful save the hook invalidates the public ["config"] query so the
 * per-site mirror panel reflects a toggled `githubMirrorEnabled`.
 *
 * Validation (react-hook-form + zod): `enabled` (switch), `org` (optional, basic
 * GitHub owner grammar), `token`/`webhookSecret` (optional write-only strings).
 */

import { useMemo, useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { GitFork, CheckCircle2 } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { FormField } from "@/components/molecules/form-field";
import { LoadingState } from "@/components/molecules/loading-state";
import { ErrorState } from "@/components/molecules/error-state";
import { Spinner } from "@/components/atoms";
import { Form } from "@/components/ui/form";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useAdminGitHub, useUpdateAdminGitHub } from "@/lib/api/hooks";
import { errorMessage } from "@/lib/api/error";
import type { GitHubAdminConfigUpdate } from "@/lib/api/types";

// GitHub owner/org grammar: 1-39 chars of [A-Za-z0-9-], no leading/trailing or
// doubled hyphen. Deliberately lenient (server is the authority) — just blocks
// obvious typos (slashes, spaces, URLs) before the round-trip. Empty is allowed
// (clears the org).
const GITHUB_ORG_RE =
  /^[A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9])){0,38}$/;

export interface GitHubAdminSectionProps {
  className?: string;
}

export function GitHubAdminSection({ className }: GitHubAdminSectionProps) {
  const t = useTranslations("settings");
  const tc = useTranslations("common");

  const configQuery = useAdminGitHub();
  const updateGitHub = useUpdateAdminGitHub();

  // Schema: org validated only when non-empty; token/secret are free-form
  // write-only strings (the server enforces real constraints).
  const schema = useMemo(
    () =>
      z.object({
        enabled: z.boolean(),
        org: z
          .string()
          .trim()
          .refine((v) => v === "" || GITHUB_ORG_RE.test(v), {
            message: t("instanceGithubOrgInvalid"),
          }),
        token: z.string(),
        webhookSecret: z.string(),
      }),
    [t],
  );

  type FormValues = z.infer<typeof schema>;

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    mode: "onChange",
    defaultValues: { enabled: false, org: "", token: "", webhookSecret: "" },
  });

  // Watch enabled/token via `useWatch` (the subscription hook) rather than
  // `form.watch()` so the values are React Compiler-safe (the returned `watch`
  // function can't be memoized — react-hooks/incompatible-library; same idiom as
  // CreateSiteForm). Drives the "enabled but no token" warning below.
  const enabledValue = useWatch({ control: form.control, name: "enabled" });
  const tokenValue = useWatch({ control: form.control, name: "token" });

  // Seed the form from the fetched config DURING render (React-recommended over
  // set-state-in-effect; the codebase idiom). We only seed `enabled`/`org` (the
  // non-secret axes); token/secret stay blank because they're write-only and
  // never returned. Re-seed whenever the configured snapshot changes.
  const config = configQuery.data;
  const [seededKey, setSeededKey] = useState<string>("");
  if (config) {
    const key = `${config.enabled}|${config.org}|${config.tokenSet}|${config.webhookSecretSet}`;
    if (key !== seededKey) {
      setSeededKey(key);
      form.reset({
        enabled: config.enabled,
        org: config.org,
        token: "",
        webhookSecret: "",
      });
    }
  }

  if (configQuery.isLoading) {
    return <LoadingState rows={4} label={t("instanceGithub")} />;
  }
  if (configQuery.isError || !config) {
    return (
      <ErrorState
        error={configQuery.error}
        title={t("instanceGithub")}
        onRetry={() => configQuery.refetch()}
      />
    );
  }

  const onSubmit = form.handleSubmit(async (values) => {
    // Build a PARTIAL update: always send enabled/org; only send token/secret
    // when the admin actually typed one (blank KEEPS the stored value — they're
    // write-only and never round-tripped).
    const body: GitHubAdminConfigUpdate = {
      enabled: values.enabled,
      org: values.org.trim(),
    };
    const token = values.token.trim();
    if (token.length > 0) body.token = token;
    const secret = values.webhookSecret.trim();
    if (secret.length > 0) body.webhookSecret = secret;

    try {
      await updateGitHub.mutateAsync(body);
      toast.success(t("instanceGithubSaved"));
      // Clear the write-only fields after a successful save so they don't linger
      // in the DOM; the freshly-returned config re-seeds enabled/org.
      form.setValue("token", "");
      form.setValue("webhookSecret", "");
    } catch (err) {
      toast.error(errorMessage(err, t("instanceGithubSaveError")));
    }
  });

  // Explicitly clear the stored DB token (reverts to env token, if any).
  const clearToken = async () => {
    try {
      await updateGitHub.mutateAsync({ clearToken: true });
      toast.success(t("instanceGithubTokenCleared"));
      form.setValue("token", "");
    } catch (err) {
      toast.error(errorMessage(err, t("instanceGithubSaveError")));
    }
  };

  const busy = updateGitHub.isPending;

  return (
    <Card className={className} aria-labelledby="settings-instance-github">
      <CardHeader>
        <CardTitle
          id="settings-instance-github"
          className="flex items-center gap-2 text-lg"
        >
          <GitFork className="size-5 shrink-0" aria-hidden="true" />
          {t("instanceGithub")}
        </CardTitle>
        <CardDescription>{t("instanceGithubDescription")}</CardDescription>
      </CardHeader>

      <CardContent>
        <Form {...form}>
          <form onSubmit={onSubmit} className="space-y-6" noValidate>
            {/* Enable mirroring — switch + label row. */}
            <FormField
              control={form.control}
              name="enabled"
              render={(field) => (
                <div className="flex items-center justify-between gap-4 rounded-lg border border-border bg-muted/30 px-3 py-2.5">
                  <div className="min-w-0 space-y-0.5">
                    <span className="text-sm font-medium text-foreground">
                      {t("instanceGithubEnable")}
                    </span>
                    <p className="text-sm text-muted-foreground">
                      {t("instanceGithubEnableHint")}
                    </p>
                  </div>
                  <Switch
                    checked={!!field.value}
                    onCheckedChange={(checked) => field.onChange(checked)}
                    aria-label={t("instanceGithubEnable")}
                    disabled={busy}
                  />
                </div>
              )}
            />

            {/* Org / owner. */}
            <FormField
              control={form.control}
              name="org"
              label={t("instanceGithubOrg")}
              description={t("instanceGithubOrgHint")}
              render={(field) => (
                <Input
                  {...field}
                  value={field.value ?? ""}
                  placeholder="necorox-com"
                  className="font-mono sm:max-w-xs"
                  autoComplete="off"
                  spellCheck={false}
                  disabled={busy}
                />
              )}
            />

            {/* PAT / push token — write-only. */}
            <FormField
              control={form.control}
              name="token"
              label={t("instanceGithubToken")}
              description={t("instanceGithubTokenHint")}
              render={(field) => (
                <div className="space-y-2">
                  {config.tokenSet ? (
                    <p className="flex items-center gap-1.5 text-sm text-success">
                      <CheckCircle2 className="size-4" aria-hidden="true" />
                      {t("instanceGithubConfigured")}
                    </p>
                  ) : null}
                  <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
                    <Input
                      {...field}
                      type="password"
                      value={field.value ?? ""}
                      placeholder={
                        config.tokenSet
                          ? t("instanceGithubTokenUpdatePlaceholder")
                          : "ghp_…"
                      }
                      className="font-mono sm:max-w-md"
                      autoComplete="off"
                      spellCheck={false}
                      disabled={busy}
                    />
                    {config.tokenSet ? (
                      <Button
                        type="button"
                        variant="outline"
                        size="sm"
                        onClick={() => void clearToken()}
                        disabled={busy}
                        className="shrink-0"
                      >
                        {t("instanceGithubTokenClear")}
                      </Button>
                    ) : null}
                  </div>
                </div>
              )}
            />

            {/* Webhook secret — write-only, optional. */}
            <FormField
              control={form.control}
              name="webhookSecret"
              label={t("instanceGithubWebhookSecret")}
              description={t("instanceGithubWebhookSecretHint")}
              render={(field) => (
                <div className="space-y-2">
                  {config.webhookSecretSet ? (
                    <p className="flex items-center gap-1.5 text-sm text-success">
                      <CheckCircle2 className="size-4" aria-hidden="true" />
                      {t("instanceGithubConfigured")}
                    </p>
                  ) : null}
                  <Input
                    {...field}
                    type="password"
                    value={field.value ?? ""}
                    placeholder={
                      config.webhookSecretSet
                        ? t("instanceGithubTokenUpdatePlaceholder")
                        : "••••••••"
                    }
                    className="font-mono sm:max-w-md"
                    autoComplete="off"
                    spellCheck={false}
                    disabled={busy}
                  />
                </div>
              )}
            />

            {/* Note: enabling without a token can't actually push. */}
            {enabledValue && !config.tokenSet && !tokenValue?.trim() ? (
              <Alert>
                <AlertDescription>
                  {t("instanceGithubNeedsToken")}
                </AlertDescription>
              </Alert>
            ) : null}

            <div className="flex justify-end">
              <Button
                type="submit"
                disabled={busy || !form.formState.isValid}
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
