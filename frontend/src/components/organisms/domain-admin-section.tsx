"use client";

/**
 * DomainAdminSection (organism) — the INSTANCE-level domain / URL config on the
 * /settings page (sibling of GitHubAdminSection). ADMIN-ONLY: the page mounts it
 * solely when `me.user.isAdmin`, and the endpoints sit behind RequireAdmin
 * server-side (GET/PUT /api/admin/domain).
 *
 * Exactly TWO settings, WordPress-style (CANONICAL product decision):
 *  - Base domain     (KOTOJI_BASE_DOMAIN)  — parses a Host into {handle}[--{branch}].<base>.
 *  - Control base URL (KOTOJI_CONTROL_BASE_URL) — external URL of the control host.
 *
 * Precedence is env OVERRIDES DB OVERRIDES request-derived default, reported per
 * field by the API as a `*Source` ("env"|"db"|"derived") + a `*Locked` flag:
 *  - source==="env"     → the env var is set; the field is READ-ONLY with a
 *    "環境変数で設定済み / Set via environment" badge + a tooltip naming the
 *    KOTOJI_* var. (The live instance sets both → both locked, GUI unchanged.)
 *  - source==="db"      → editable; the saved value.
 *  - source==="derived" → editable; a placeholder value derived from THIS request
 *    on a fresh install (we surface a hint nudging the admin to set the real one).
 *
 * The values are NOT secret, so GET returns them verbatim and the inputs are
 * seeded with the effective value (unlike the write-only GitHub secrets). On a
 * successful PUT the hook invalidates ["config"] (it exposes baseDomain) + the
 * domain query, and we toast. PUT may reject a LOCKED field (409 conflict) or an
 * INVALID value (422 validation); both carry `details.field`, which we map back
 * onto the offending input inline.
 *
 * Validation (react-hook-form + zod) mirrors the server: base domain = a bare DNS
 * hostname (no scheme/port/path), control base URL = an absolute http(s) origin.
 * Both allow empty (an empty save REVERTS that field to the env/derived fallback).
 */

import { useMemo, useState } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import { Globe, Lock, TriangleAlert } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { FormField } from "@/components/molecules/form-field";
import { LoadingState } from "@/components/molecules/loading-state";
import { ErrorState } from "@/components/molecules/error-state";
import { Spinner } from "@/components/atoms";
import { Form } from "@/components/ui/form";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
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
import { useAdminDomain, useUpdateAdminDomain } from "@/lib/api/hooks";
import { errorMessage, isApiError } from "@/lib/api/error";
import type { DomainAdminConfigUpdate } from "@/lib/api/types";

// Bare DNS hostname: dot-separated labels of [A-Za-z0-9-] (no leading/trailing or
// doubled hyphen per label), at least one dot. Deliberately lenient — the server
// is the authority (ValidateBaseDomain); this just blocks obvious mistakes
// (scheme, port, path, spaces) before the round-trip. Empty is allowed (reverts).
const HOSTNAME_RE =
  /^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/;

export interface DomainAdminSectionProps {
  className?: string;
}

export function DomainAdminSection({ className }: DomainAdminSectionProps) {
  const t = useTranslations("settings");
  const tc = useTranslations("common");

  const configQuery = useAdminDomain();
  const updateDomain = useUpdateAdminDomain();

  // Schema mirrors the server: base domain is a bare hostname; control base URL
  // is an absolute http(s) origin (no path/query/fragment). Both allow empty —
  // an empty save deliberately reverts that field to the env/derived fallback.
  const schema = useMemo(
    () =>
      z.object({
        baseDomain: z
          .string()
          .trim()
          .refine((v) => v === "" || HOSTNAME_RE.test(v), {
            message: t("instanceDomainBaseDomainInvalid"),
          }),
        controlBaseURL: z
          .string()
          .trim()
          .refine((v) => v === "" || isHttpOrigin(v), {
            message: t("instanceDomainControlBaseURLInvalid"),
          }),
      }),
    [t],
  );

  type FormValues = z.infer<typeof schema>;

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    mode: "onChange",
    defaultValues: { baseDomain: "", controlBaseURL: "" },
  });

  // Seed the form from the fetched config DURING render (codebase idiom; React-
  // recommended over set-state-in-effect). The values are NOT secret, so we seed
  // the effective value verbatim. Re-seed whenever the snapshot changes.
  const config = configQuery.data;
  const [seededKey, setSeededKey] = useState<string>("");
  if (config) {
    const key = `${config.baseDomain}|${config.controlBaseURL}|${config.baseDomainSource}|${config.controlBaseURLSource}`;
    if (key !== seededKey) {
      setSeededKey(key);
      form.reset({
        baseDomain: config.baseDomain,
        controlBaseURL: config.controlBaseURL,
      });
    }
  }

  if (configQuery.isLoading) {
    return <LoadingState rows={4} label={t("instanceDomain")} />;
  }
  if (configQuery.isError || !config) {
    return (
      <ErrorState
        error={configQuery.error}
        title={t("instanceDomain")}
        onRetry={() => configQuery.refetch()}
      />
    );
  }

  const baseLocked = config.baseDomainLocked;
  const controlLocked = config.controlBaseURLLocked;

  const onSubmit = form.handleSubmit(async (values) => {
    // Build a PARTIAL update: send ONLY editable (non-locked) fields, and only
    // when the value actually changed from the seeded effective value — this
    // avoids a needless write and avoids tripping the server's locked-field 409
    // for a field the admin never touched. An empty string is a deliberate
    // revert (kept, since "" differs from a non-empty effective value).
    const body: DomainAdminConfigUpdate = {};
    const nextBase = values.baseDomain.trim();
    if (!baseLocked && nextBase !== config.baseDomain) {
      body.baseDomain = nextBase;
    }
    const nextControl = values.controlBaseURL.trim();
    if (!controlLocked && nextControl !== config.controlBaseURL) {
      body.controlBaseURL = nextControl;
    }

    // Nothing changed — short-circuit (no toast spam, no write).
    if (body.baseDomain === undefined && body.controlBaseURL === undefined) {
      return;
    }

    try {
      await updateDomain.mutateAsync(body);
      toast.success(t("instanceDomainSaved"));
    } catch (err) {
      // Map a server field error (409 locked / 422 invalid) back onto the input
      // inline; both carry `details.field`. Fall back to a toast otherwise.
      if (isApiError(err)) {
        const field = fieldFromDetails(err.details);
        if (field) {
          form.setError(field, {
            message:
              err.code === "conflict"
                ? t("instanceDomainLockedError")
                : err.message ||
                  (field === "baseDomain"
                    ? t("instanceDomainBaseDomainInvalid")
                    : t("instanceDomainControlBaseURLInvalid")),
          });
          return;
        }
      }
      toast.error(errorMessage(err, t("instanceDomainSaveError")));
    }
  });

  const busy = updateDomain.isPending;
  // Disable submit when nothing differs from the seeded effective value, so a
  // pristine (or all-locked) form can't fire a no-op request.
  const dirty = form.formState.isDirty;

  return (
    <Card className={className} aria-labelledby="settings-instance-domain">
      <CardHeader>
        <CardTitle
          id="settings-instance-domain"
          className="flex items-center gap-2 text-lg"
        >
          <Globe className="size-5 shrink-0" aria-hidden="true" />
          {t("instanceDomain")}
        </CardTitle>
        <CardDescription>{t("instanceDomainDescription")}</CardDescription>
      </CardHeader>

      <CardContent>
        {/* WordPress-honest caveat: DNS + TLS live OUTSIDE the app, and changing
            the control base URL can sign you out (cookie domain change). */}
        <Alert className="mb-6">
          <TriangleAlert className="size-4" aria-hidden="true" />
          <AlertTitle>{t("instanceDomainNoteTitle")}</AlertTitle>
          <AlertDescription>
            <ul className="list-disc space-y-1 pl-4">
              <li>{t("instanceDomainNoteDns")}</li>
              <li>{t("instanceDomainNoteCookie")}</li>
            </ul>
          </AlertDescription>
        </Alert>

        <Form {...form}>
          <form onSubmit={onSubmit} className="space-y-6" noValidate>
            {/* Base domain. */}
            <FormField
              control={form.control}
              name="baseDomain"
              label={
                <span className="flex items-center gap-2">
                  {t("instanceDomainBaseDomain")}
                  {baseLocked ? (
                    <LockedBadge
                      tooltip={t("instanceDomainLockedTooltipBaseDomain")}
                      label={t("instanceDomainLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("instanceDomainBaseDomainHint")}
              render={(field) => (
                <Input
                  {...field}
                  value={field.value ?? ""}
                  placeholder="hosting.example.com"
                  className="font-mono sm:max-w-md"
                  autoComplete="off"
                  spellCheck={false}
                  inputMode="url"
                  disabled={busy || baseLocked}
                  readOnly={baseLocked}
                  aria-readonly={baseLocked}
                />
              )}
            />
            {/* Derived-state nudge (fresh install): the value is a placeholder. */}
            {!baseLocked && config.baseDomainSource === "derived" ? (
              <p className="-mt-3 text-sm text-muted-foreground">
                {t("instanceDomainDerivedHint")}
              </p>
            ) : null}

            {/* Control base URL. */}
            <FormField
              control={form.control}
              name="controlBaseURL"
              label={
                <span className="flex items-center gap-2">
                  {t("instanceDomainControlBaseURL")}
                  {controlLocked ? (
                    <LockedBadge
                      tooltip={t("instanceDomainLockedTooltipControlBaseURL")}
                      label={t("instanceDomainLockedBadge")}
                    />
                  ) : null}
                </span>
              }
              description={t("instanceDomainControlBaseURLHint")}
              render={(field) => (
                <Input
                  {...field}
                  value={field.value ?? ""}
                  placeholder="https://hosting.example.com"
                  className="font-mono sm:max-w-md"
                  autoComplete="off"
                  spellCheck={false}
                  inputMode="url"
                  disabled={busy || controlLocked}
                  readOnly={controlLocked}
                  aria-readonly={controlLocked}
                />
              )}
            />
            {!controlLocked && config.controlBaseURLSource === "derived" ? (
              <p className="-mt-3 text-sm text-muted-foreground">
                {t("instanceDomainDerivedHint")}
              </p>
            ) : null}

            <div className="flex justify-end">
              <Button
                type="submit"
                disabled={
                  busy ||
                  !form.formState.isValid ||
                  !dirty ||
                  (baseLocked && controlLocked)
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
 * isHttpOrigin — true when `v` parses as an absolute http(s) URL with no path,
 * query, or fragment (a bare origin). Mirrors the server's ValidateControlBaseURL
 * intent; the server stays the authority (it also normalizes a trailing slash).
 */
function isHttpOrigin(v: string): boolean {
  let u: URL;
  try {
    u = new URL(v);
  } catch {
    return false;
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return false;
  if (!u.hostname) return false;
  // Reject anything beyond the bare origin (a trailing "/" is tolerated — the
  // server normalizes it away; "/auth" or "?x"/"#y" are not).
  if (u.pathname !== "" && u.pathname !== "/") return false;
  if (u.search !== "" || u.hash !== "") return false;
  return true;
}

/**
 * fieldFromDetails — pull a known form field name out of a server error's
 * `details` ({ field, reason }). Returns undefined for an unrecognized field so
 * the caller falls back to a toast.
 */
function fieldFromDetails(
  details: Record<string, unknown> | null,
): "baseDomain" | "controlBaseURL" | undefined {
  const field = details?.field;
  if (field === "baseDomain" || field === "controlBaseURL") return field;
  return undefined;
}
