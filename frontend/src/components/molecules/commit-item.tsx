"use client";

/**
 * CommitItem (molecule) — design.md §3.2. One history entry: author Avatar +
 * message + short SHA (CodeText) + relative time + SourceTag (provenance). The
 * row exposes "差分 / View diff" and (optionally) "戻す / Roll back" actions the
 * HistoryTimeline wires to its mutations. Pure props; no fetching.
 */

import { useFormatter, useTranslations } from "next-intl";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import { CodeText } from "@/components/atoms";
import { SourceTag } from "./source-tag";
import type { CommitInfo } from "@/lib/api/types";
import { cn } from "@/lib/utils";

export interface CommitItemProps {
  commit: CommitInfo;
  onViewDiff?: (sha: string) => void;
  onRollback?: (sha: string) => void;
  className?: string;
}

/** Two-letter initials from an author name for the Avatar fallback. */
function initials(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return "?";
  const parts = trimmed.split(/\s+/);
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

export function CommitItem({
  commit,
  onViewDiff,
  onRollback,
  className,
}: CommitItemProps) {
  const t = useTranslations("history");
  const format = useFormatter();
  const relative = format.relativeTime(new Date(commit.committed));

  return (
    <div
      data-slot="commit-item"
      className={cn(
        "flex items-start gap-3 rounded-lg px-2 py-2 transition-colors hover:bg-accent/50",
        className
      )}
    >
      <Avatar size="sm" className="mt-0.5 shrink-0">
        <AvatarFallback>{initials(commit.authorName)}</AvatarFallback>
      </Avatar>
      <div className="min-w-0 flex-1 space-y-1">
        <p className="truncate text-sm font-medium text-foreground">
          {commit.message}
        </p>
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <CodeText className="bg-transparent px-0">{commit.shortSha}</CodeText>
          <SourceTag source={commit.via} />
          <span>{relative}</span>
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-1">
        {onViewDiff ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => onViewDiff(commit.sha)}
          >
            {t("viewDiff")}
          </Button>
        ) : null}
        {onRollback ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => onRollback(commit.sha)}
          >
            {t("rollback")}
          </Button>
        ) : null}
      </div>
    </div>
  );
}
