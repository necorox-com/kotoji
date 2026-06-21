"use client";

/**
 * ConflictResolver (organism) — design.md §3.3 / §4.1, CANONICAL.md §3 + §8.
 *
 * Shown when a Save returns a 409 optimistic-lock conflict: someone (or an AI)
 * changed the file under the user's edit. It renders the FROZEN ConflictError
 * shape `{ branch, expected, actual, changedPaths }` in plain language and offers
 * two paths:
 *
 *   - RELOAD (safe default): discard the user's edit, take the server's latest.
 *   - OVERWRITE (secondary, confirmed): re-save the user's content against the
 *     NEW server tip (`actual`) so the write succeeds and supersedes the server
 *     change. Confirmed via ConfirmDialog because it discards the other change.
 *
 * The yours↔server comparison uses DiffViewer in CONTENT mode. The server's
 * current content is fetched fresh (useFileContent has no ref → branch tip).
 */

import { useState } from "react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";
import { RotateCcw, TriangleAlert, Upload } from "lucide-react";
import { useFileContent, useWriteFile } from "@/lib/api/hooks";
import { errorMessage, type ConflictError } from "@/lib/api/error";
import {
  Alert,
  AlertDescription,
  AlertTitle,
} from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/atoms/spinner";
import { CodeText } from "@/components/atoms/code-text";
import { ConfirmDialog } from "@/components/molecules/confirm-dialog";
import { LoadingState } from "@/components/molecules/loading-state";
import { ErrorState } from "@/components/molecules/error-state";
import { DiffViewer } from "./diff-viewer";
import { cn } from "@/lib/utils";

export interface ConflictResolverProps {
  handle: string;
  branch: string;
  /** The file whose save conflicted. */
  path: string;
  /** The typed conflict (carries expected/actual/changedPaths, CANONICAL §8). */
  conflict: ConflictError;
  /** The content the user tried to save (their version). */
  attemptedContent: string;
  /**
   * Reload to the server version: parent should re-open the file (re-seeding the
   * editor with the new baseSha). The organism only signals intent.
   */
  onReload: () => void;
  /** Called after a successful overwrite so the parent can close + re-seed. */
  onResolved?: () => void;
  /** Dismiss without choosing. */
  onCancel?: () => void;
  className?: string;
}

export function ConflictResolver({
  handle,
  branch,
  path,
  conflict,
  attemptedContent,
  onReload,
  onResolved,
  onCancel,
  className,
}: ConflictResolverProps) {
  const t = useTranslations("conflict");
  const tCommon = useTranslations("common");
  const [confirmOpen, setConfirmOpen] = useState(false);

  // Fetch the server's CURRENT content (branch tip) to diff against the user's
  // attempted version. No `ref` → the hook reads the live tip.
  const server = useFileContent(handle, branch, path);
  const write = useWriteFile(handle, branch);

  const doOverwrite = async () => {
    try {
      // Re-save the user's content against the NEW tip (`actual`) so the write
      // is no longer stale and supersedes the server change.
      await write.mutateAsync({
        path,
        content: attemptedContent,
        baseSha: conflict.actual,
      });
      toast.success(tCommon("saved"));
      setConfirmOpen(false);
      onResolved?.();
    } catch (err) {
      // A second conflict can occur if the tip moved again; surface and let the
      // parent re-trigger the resolver with the fresh error if needed.
      toast.error(errorMessage(err, t("title")));
      setConfirmOpen(false);
    }
  };

  return (
    <div
      data-slot="conflict-resolver"
      role="region"
      aria-label={t("title")}
      className={cn("flex min-h-0 flex-col gap-4", className)}
    >
      {/* Plain-language explanation + the conflicting paths. */}
      <Alert variant="destructive">
        <TriangleAlert aria-hidden="true" />
        <AlertTitle>{t("title")}</AlertTitle>
        <AlertDescription>
          <p>{t("body")}</p>
          {conflict.changedPaths.length > 0 ? (
            <div className="mt-2">
              <p className="font-medium text-foreground">{t("changedFiles")}</p>
              <ul className="mt-1 flex flex-wrap gap-1.5">
                {conflict.changedPaths.map((p) => (
                  <li key={p}>
                    <CodeText>{p}</CodeText>
                  </li>
                ))}
              </ul>
            </div>
          ) : null}
        </AlertDescription>
      </Alert>

      {/* yours ↔ server diff (DiffViewer CONTENT mode). */}
      <div className="min-h-0 flex-1">
        {server.isPending ? (
          <LoadingState rows={6} label={t("server")} />
        ) : server.isError ? (
          <ErrorState
            error={server.error}
            onRetry={() => void server.refetch()}
          />
        ) : (
          <DiffViewer
            mode="content"
            path={path}
            original={server.data?.content ?? ""}
            modified={attemptedContent}
            fromLabel={t("server")}
            toLabel={t("yours")}
            height={360}
          />
        )}
      </div>

      {/* Actions: reload (safe default, primary) + overwrite (secondary). */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="space-y-2">
          <Button type="button" onClick={onReload}>
            <RotateCcw className="size-3.5" aria-hidden="true" />
            {t("reload")}
          </Button>
          <p className="text-xs text-muted-foreground">{t("reloadHint")}</p>
        </div>
        <div className="space-y-2 sm:text-right">
          <Button
            type="button"
            variant="outline"
            onClick={() => setConfirmOpen(true)}
            disabled={server.isPending}
          >
            {write.isPending ? (
              <Spinner size="sm" />
            ) : (
              <Upload className="size-3.5" aria-hidden="true" />
            )}
            {t("overwrite")}
          </Button>
          <p className="text-xs text-muted-foreground">{t("overwriteHint")}</p>
        </div>
      </div>

      {onCancel ? (
        <div>
          <Button type="button" variant="ghost" size="sm" onClick={onCancel}>
            {tCommon("cancel")}
          </Button>
        </div>
      ) : null}

      {/* Overwrite is destructive-ish (discards the other change) → confirm. */}
      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title={t("overwrite")}
        description={t("overwriteHint")}
        confirmLabel={t("overwrite")}
        variant="destructive"
        loading={write.isPending}
        onConfirm={doOverwrite}
      />
    </div>
  );
}
