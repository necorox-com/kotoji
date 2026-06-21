"use client";

/**
 * PublishPanel (organism) — design.md §3.3 / §3.5 ProjectDetail · Publish.
 *
 * The Publish tab/flow. It:
 *  - shows WHAT WILL CHANGE (draft↔published name-status summary via useDiff),
 *  - shows the current publish state (published / draft-ahead),
 *  - takes an optional publish message,
 *  - warns plainly ("公開すると … に即反映されます。後で戻せます。"),
 *  - requires a ConfirmDialog,
 *  - runs usePublish with the source-tip baseSha (optimistic lock),
 *  - on success: celebratory toast (+ gold glint, motion-safe),
 *  - on the two distinct 409s (stale base / merge conflict) shows the right
 *    plain-language inline explanation (CANONICAL.md §3; error.ts typed errors).
 *
 * Role/mode gating (CANONICAL.md §6.1 + publish_mode): viewers can't publish;
 * editors under publish_mode='request' see "公開をリクエスト". Driven by the
 * capabilities helper so the contract is the single source of truth.
 *
 * Mobile-first: single column; the diff summary list wraps; the publish CTA is
 * full-width on phones.
 */

import { useState } from "react";
import { CircleCheck, Rocket, TriangleAlert } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/molecules/confirm-dialog";
import { EmptyState } from "@/components/molecules/empty-state";
import { ErrorState } from "@/components/molecules/error-state";
import { LoadingState } from "@/components/molecules/loading-state";
import { CodeText } from "@/components/atoms/code-text";
import { CopyableUrl } from "@/components/molecules/copyable-url";
import { StatusBadge } from "@/components/atoms/status-badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { usePublish, useDiff, useBranches } from "@/lib/api/hooks";
import {
  isPublishConflictError,
  isConflictError,
  errorMessage,
} from "@/lib/api/error";
import { canPublish, isPublishRequest } from "@/lib/api/capabilities";
import type { FileDiff, PublishMode, SiteRole } from "@/lib/api/types";
import { cn } from "@/lib/utils";

const PUBLISHED = "published";

export interface PublishPanelProps {
  handle: string;
  /** Source branch to publish (defaults to "draft"). */
  from?: string;
  role: SiteRole;
  publishMode: PublishMode;
  baseDomain: string;
  className?: string;
}

/** Sum additions/deletions across changed files for the DiffStat summary. */
function diffTotals(files: FileDiff[]): { adds: number; dels: number } {
  return files.reduce(
    (acc, f) => ({
      adds: acc.adds + (f.additions ?? 0),
      dels: acc.dels + (f.deletions ?? 0),
    }),
    { adds: 0, dels: 0 },
  );
}

/** Map a FileDiff status to a status-badge variant (added/modified/deleted). */
function statusFor(
  status: FileDiff["status"],
): "published" | "draft" | "error" {
  if (status === "added") return "published";
  if (status === "deleted") return "error";
  return "draft";
}

export function PublishPanel({
  handle,
  from = "draft",
  role,
  publishMode,
  baseDomain,
  className,
}: PublishPanelProps) {
  const t = useTranslations("publish");
  const tc = useTranslations("common");
  const tAuth = useTranslations("auth");

  // The diff that PREVIEWS what publishing will change (name-status is enough
  // for the summary; design §3.5 shows a DiffViewer summary + DiffStat).
  const diffQuery = useDiff(handle, from, PUBLISHED, { nameStatus: true });
  // We need the tip of `from` as the optimistic-lock baseSha for publish.
  const branchesQuery = useBranches(handle);
  const publish = usePublish(handle);

  const [message, setMessage] = useState("");
  const [confirmOpen, setConfirmOpen] = useState(false);

  const allowed = canPublish(role, publishMode);
  const asRequest = isPublishRequest(role, publishMode);

  const fromBranch = branchesQuery.data?.find((b) => b.name === from);
  const baseSha = fromBranch?.headSha ?? "";
  const publishedUrl = `${handle}.${baseDomain}`;

  const files = diffQuery.data?.files ?? [];
  const hasChanges = files.length > 0;
  const totals = diffTotals(files);

  // The two distinct publish 409s drive different plain-language explanations.
  const err = publish.error;
  const mergeConflict = isPublishConflictError(err) ? err : null;
  const staleConflict = isConflictError(err) ? err : null;

  const runPublish = async () => {
    try {
      const result = await publish.mutateAsync({
        from,
        baseSha,
        ...(message.trim() ? { message: message.trim() } : {}),
      });
      // Celebratory success (the gold glint is the StatusBadge published color +
      // toast; motion is globally reduced-motion-safe per globals.css §4.7).
      toast.success(t("success"));
      setConfirmOpen(false);
      setMessage("");
      void result;
    } catch (e) {
      // Keep the confirm dialog open on a conflict so the inline explanation
      // (rendered below) is read; otherwise close and surface a toast.
      if (isConflictError(e) || isPublishConflictError(e)) {
        setConfirmOpen(false);
      } else {
        setConfirmOpen(false);
        toast.error(errorMessage(e, t("error")));
      }
    }
  };

  // ---- loading / error gates for the diff preview ----
  if (diffQuery.isLoading || branchesQuery.isLoading) {
    return (
      <section className={cn("space-y-4", className)} data-slot="publish-panel">
        <LoadingState rows={3} label={t("title")} />
      </section>
    );
  }

  if (diffQuery.isError) {
    return (
      <section className={cn("space-y-4", className)} data-slot="publish-panel">
        <ErrorState
          error={diffQuery.error}
          title={t("title")}
          onRetry={() => diffQuery.refetch()}
        />
      </section>
    );
  }

  return (
    <section
      data-slot="publish-panel"
      className={cn("space-y-6", className)}
      aria-labelledby="publish-panel-heading"
    >
      {/* Header + current state */}
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2
          id="publish-panel-heading"
          className="text-xl font-semibold text-foreground"
        >
          {t("title")}
        </h2>
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <span>{t("current")}:</span>
          <StatusBadge status={hasChanges ? "draft" : "published"} />
        </div>
      </header>

      {/* Published URL */}
      <div className="flex items-center gap-2 text-sm">
        <span className="text-muted-foreground">{tc("open")}:</span>
        <CopyableUrl value={publishedUrl} href={`https://${publishedUrl}`} />
      </div>

      {/* Conflict explanations (typed, plain-language) */}
      {mergeConflict ? (
        <div
          role="alert"
          className="rounded-lg border border-warning/40 bg-warning/10 px-3 py-2.5 text-sm"
        >
          <p className="flex items-center gap-2 font-medium text-warning-foreground dark:text-warning">
            <TriangleAlert className="size-4 shrink-0" aria-hidden="true" />
            {t("conflictTitle")}
          </p>
          <p className="mt-1 text-muted-foreground">{t("mergeConflictBody")}</p>
          <ul className="mt-1.5 flex flex-wrap gap-1.5">
            {mergeConflict.paths.map((p) => (
              <li key={p}>
                <CodeText>{p}</CodeText>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {staleConflict ? (
        <div
          role="alert"
          className="rounded-lg border border-warning/40 bg-warning/10 px-3 py-2.5 text-sm"
        >
          <p className="flex items-center gap-2 font-medium text-warning-foreground dark:text-warning">
            <TriangleAlert className="size-4 shrink-0" aria-hidden="true" />
            {t("conflictTitle")}
          </p>
          <p className="mt-1 text-muted-foreground">{t("conflictBody")}</p>
        </div>
      ) : null}

      {/* What will change */}
      <div className="space-y-2">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-medium text-foreground">
            {t("changesToPublish")}
          </h3>
          {hasChanges ? (
            <span className="flex items-center gap-2 font-mono text-xs">
              <span className="text-success">+{totals.adds}</span>
              <span className="text-destructive">-{totals.dels}</span>
            </span>
          ) : null}
        </div>

        {hasChanges ? (
          <ul className="divide-y divide-border rounded-lg border border-border">
            {files.map((f) => (
              <li
                key={f.path}
                className="flex items-center justify-between gap-3 px-3 py-2 text-sm"
              >
                <span className="flex min-w-0 items-center gap-2">
                  <StatusBadge status={statusFor(f.status)} label={f.status} />
                  <CodeText truncate className="min-w-0">
                    {f.path}
                  </CodeText>
                </span>
                <span className="shrink-0 font-mono text-xs">
                  <span className="text-success">+{f.additions}</span>{" "}
                  <span className="text-destructive">-{f.deletions}</span>
                </span>
              </li>
            ))}
          </ul>
        ) : (
          <EmptyState
            icon={CircleCheck}
            title={t("nothingToPublish")}
            body={t("confirmBody")}
          />
        )}
      </div>

      {/* Publish message + warning + CTA (only when there is something to do) */}
      {hasChanges && allowed ? (
        <div className="space-y-4">
          <div className="grid gap-2">
            <Label htmlFor="publish-message">{t("messageLabel")}</Label>
            <Textarea
              id="publish-message"
              value={message}
              onChange={(e) => setMessage(e.target.value)}
              placeholder={t("messagePlaceholder")}
            />
          </div>

          <p className="flex items-start gap-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2 text-sm text-muted-foreground">
            <TriangleAlert
              className="mt-0.5 size-4 shrink-0 text-warning-foreground dark:text-warning"
              aria-hidden="true"
            />
            <span>{t("warning", { url: publishedUrl })}</span>
          </p>

          <div className="flex flex-col gap-2 sm:flex-row sm:justify-end">
            <Button
              // Gold-ringed publish CTA (design.md §3.1 publish variant intent).
              className="ring-1 ring-inset ring-brand-gold/40"
              onClick={() => setConfirmOpen(true)}
              disabled={publish.isPending || baseSha.length === 0}
              aria-busy={publish.isPending}
            >
              <Rocket aria-hidden="true" />
              {asRequest ? t("requestPublish") : t("publish")}
            </Button>
          </div>
        </div>
      ) : null}

      {/* Viewer (or otherwise un-permitted) note — plain read-only reason */}
      {hasChanges && !allowed ? (
        <p className="text-sm text-muted-foreground">
          {tAuth("notAuthorized")}
        </p>
      ) : null}

      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title={t("confirmTitle")}
        description={t("confirmBody")}
        confirmLabel={asRequest ? t("requestPublish") : t("publish")}
        onConfirm={runPublish}
        loading={publish.isPending}
      />
    </section>
  );
}
