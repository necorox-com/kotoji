"use client";

/**
 * LoadingState (molecule) — design.md §4.2 (mandatory loading/error/empty
 * triplet). A centered Spinner + optional label, OR a skeleton block when the
 * caller passes `rows`. Use the skeleton form to mirror final layout; use the
 * spinner form for small inline waits.
 */

import { useTranslations } from "next-intl";
import { Spinner } from "@/components/atoms";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

export interface LoadingStateProps {
  label?: string;
  /** When set, render N skeleton rows instead of a spinner. */
  rows?: number;
  className?: string;
}

export function LoadingState({ label, rows, className }: LoadingStateProps) {
  const t = useTranslations("common");

  if (rows && rows > 0) {
    return (
      <div
        className={cn("space-y-3", className)}
        role="status"
        aria-label={label ?? t("loading")}
      >
        {Array.from({ length: rows }).map((_, i) => (
          <Skeleton key={i} className="h-12 w-full rounded-lg" />
        ))}
        <span className="sr-only">{label ?? t("loading")}</span>
      </div>
    );
  }

  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center gap-3 py-12 text-muted-foreground",
        className
      )}
    >
      <Spinner size="lg" label={label ?? t("loading")} />
      {label ? <p className="text-sm">{label}</p> : null}
    </div>
  );
}
