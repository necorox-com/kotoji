"use client";

/**
 * ErrorState (molecule) — design.md §4.2/§4.8. The error half of the
 * loading/error/empty triplet. Shows a friendly icon + the server's safe
 * message (or a generic fallback) + a Retry button wired to refetch. Uses the
 * typed ApiError message when available (error.ts errorMessage()).
 */

import { TriangleAlert } from "lucide-react";
import { useTranslations } from "next-intl";
import { Button } from "@/components/ui/button";
import { errorMessage } from "@/lib/api/error";
import { cn } from "@/lib/utils";

export interface ErrorStateProps {
  /** The thrown error (ApiError or otherwise). */
  error?: unknown;
  /** Explicit title; defaults to a generic i18n error. */
  title?: string;
  /** Retry handler (typically a query's refetch). */
  onRetry?: () => void;
  className?: string;
}

export function ErrorState({
  error,
  title,
  onRetry,
  className,
}: ErrorStateProps) {
  const t = useTranslations();
  const message = errorMessage(error, t("errors.generic"));
  return (
    <div
      role="alert"
      className={cn(
        "flex flex-col items-center justify-center gap-3 py-12 text-center",
        className
      )}
    >
      <TriangleAlert
        className="size-6 text-destructive"
        aria-hidden="true"
      />
      <div className="space-y-1">
        <p className="font-medium text-foreground">
          {title ?? t("errors.generic")}
        </p>
        <p className="text-sm text-muted-foreground">{message}</p>
      </div>
      {onRetry ? (
        <Button variant="outline" size="sm" onClick={onRetry}>
          {t("common.retry")}
        </Button>
      ) : null}
    </div>
  );
}
