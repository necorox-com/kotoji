"use client";

/**
 * HistoryTimeline (organism) — design.md §3.3 / §3.5 ProjectDetail · History.
 *
 * The git-log view. It:
 *  - lists CommitItem rows grouped by date band (今日 / 昨日 / older),
 *  - offers a branch ("version") filter and a source/provenance filter,
 *  - opens a commit's diff via onViewDiff (the page mounts DiffViewer),
 *  - rolls a branch back to a chosen commit's tree via useRollback, behind a
 *    ConfirmDialog (a NEW forward commit — never a history rewrite; CANONICAL §1
 *    Rollback). The rollback baseSha is the current branch tip (optimistic lock).
 *
 * Role gating (CANONICAL §6.1): only owner/editor see "戻す / Roll back"; viewers
 * get read-only history + diff. Loading/error/empty triplet via the molecules.
 *
 * Mobile-first: the filters stack above the list; CommitItem already wraps its
 * meta row at narrow widths.
 */

import { useMemo, useState } from "react";
import { useFormatter, useTranslations } from "next-intl";
import { toast } from "sonner";

import { CommitItem } from "@/components/molecules/commit-item";
import { ConfirmDialog } from "@/components/molecules/confirm-dialog";
import { EmptyState } from "@/components/molecules/empty-state";
import { ErrorState } from "@/components/molecules/error-state";
import { LoadingState } from "@/components/molecules/loading-state";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useBranches, useLog, useRollback } from "@/lib/api/hooks";
import { errorMessage } from "@/lib/api/error";
import { roleCan } from "@/lib/api/capabilities";
import type { CommitInfo, SiteRole, WriteSource } from "@/lib/api/types";
import { cn } from "@/lib/utils";

// Sentinel for "all sources" in the source filter (never collides with an enum).
const ALL_SOURCES = "__all__";
const SOURCE_VALUES: WriteSource[] = ["upload", "editor", "mcp", "system"];
// One generous page of history; the backend clamps/defaults its own limit too.
const HISTORY_LIMIT = 100;

export interface HistoryTimelineProps {
  handle: string;
  /** The branch ("version") whose history is shown; parent owns the state. */
  branch: string;
  onBranchChange: (branch: string) => void;
  role: SiteRole;
  /** Open the diff for a commit (page mounts DiffViewer). */
  onViewDiff?: (sha: string) => void;
  className?: string;
}

/** Calendar-day key (local) used to bucket commits into date bands. */
function dayKey(d: Date): string {
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

/** Localized label for a source enum value (used by the dropdown rows). */
function SourceLabel({ source }: { source: WriteSource }) {
  const t = useTranslations("source");
  return <span>{t(source)}</span>;
}

export function HistoryTimeline({
  handle,
  branch,
  onBranchChange,
  role,
  onViewDiff,
  className,
}: HistoryTimelineProps) {
  const t = useTranslations("history");
  const tSource = useTranslations("source");
  const tCommon = useTranslations("common");
  const format = useFormatter();

  const branchesQuery = useBranches(handle);
  const logQuery = useLog(handle, branch, { limit: HISTORY_LIMIT });
  const rollback = useRollback(handle, branch);

  const [sourceFilter, setSourceFilter] = useState<string>(ALL_SOURCES);
  const [rollbackSha, setRollbackSha] = useState<string | null>(null);
  // "Now" captured once on mount (stable) so the date-band grouping memo stays
  // pure — no impure Date.now()/new Date() inside the render/useMemo body.
  const [nowMs] = useState(() => Date.now());

  const canRollback = roleCan(role, "write");

  // Apply the source/provenance filter client-side over the loaded page.
  const commits = useMemo(() => {
    const all = logQuery.data?.commits ?? [];
    if (sourceFilter === ALL_SOURCES) return all;
    return all.filter((c) => c.via === sourceFilter);
  }, [logQuery.data, sourceFilter]);

  // Group into date bands (今日 / 昨日 / explicit date) preserving newest-first.
  const groups = useMemo(() => {
    const today = dayKey(new Date(nowMs));
    const yesterday = dayKey(new Date(nowMs - 86_400_000));
    const ordered: { key: string; label: string; commits: CommitInfo[] }[] = [];
    const index = new Map<string, number>();

    for (const c of commits) {
      const d = new Date(c.committed);
      const key = dayKey(d);
      let label: string;
      if (key === today) label = t("today");
      else if (key === yesterday) label = t("yesterday");
      else label = format.dateTime(d, { dateStyle: "medium" });

      let pos = index.get(key);
      if (pos === undefined) {
        pos = ordered.length;
        index.set(key, pos);
        ordered.push({ key, label, commits: [] });
      }
      ordered[pos].commits.push(c);
    }
    return ordered;
  }, [commits, format, t, nowMs]);

  const branches = branchesQuery.data ?? [];
  const tip = branches.find((b) => b.name === branch)?.headSha ?? "";

  const confirmRollback = async () => {
    if (!rollbackSha) return;
    try {
      await rollback.mutateAsync({ toSha: rollbackSha, baseSha: tip });
      toast.success(t("rolledBack"));
      setRollbackSha(null);
    } catch (err) {
      toast.error(errorMessage(err, t("rollbackError")));
    }
  };

  return (
    <section
      data-slot="history-timeline"
      className={cn("space-y-4", className)}
      aria-labelledby="history-heading"
    >
      <header className="flex flex-wrap items-center justify-between gap-3">
        <h2
          id="history-heading"
          className="text-xl font-semibold text-foreground"
        >
          {t("title")}
        </h2>
        <div className="flex flex-wrap items-center gap-2">
          {/* Version filter */}
          <Select
            value={branch}
            onValueChange={(v) => v != null && onBranchChange(v)}
          >
            <SelectTrigger aria-label={t("filterBranch")} className="min-w-36">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {branches.map((b) => (
                <SelectItem key={b.name} value={b.name}>
                  <span className="truncate">{b.name}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>

          {/* Source / provenance filter */}
          <Select
            value={sourceFilter}
            onValueChange={(v) => v != null && setSourceFilter(v)}
          >
            <SelectTrigger aria-label={t("filterSource")} className="min-w-32">
              <SelectValue>
                {(v: string) =>
                  v === ALL_SOURCES
                    ? t("filterSource")
                    : tSource(v as WriteSource)
                }
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL_SOURCES}>
                <span>{tCommon("all")}</span>
              </SelectItem>
              {SOURCE_VALUES.map((s) => (
                <SelectItem key={s} value={s}>
                  <SourceLabel source={s} />
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </header>

      {/* loading / error / empty / content */}
      {logQuery.isLoading ? (
        <LoadingState rows={4} label={t("title")} />
      ) : logQuery.isError ? (
        <ErrorState
          error={logQuery.error}
          title={t("loadError")}
          onRetry={() => logQuery.refetch()}
        />
      ) : commits.length === 0 ? (
        <EmptyState title={t("empty.title")} body={t("empty.body")} />
      ) : (
        <div className="space-y-6">
          {groups.map((group) => (
            <div key={group.key} className="space-y-1">
              <h3 className="px-2 text-xs font-medium tracking-wide text-muted-foreground">
                {group.label}
              </h3>
              <ul className="space-y-0.5">
                {group.commits.map((commit) => (
                  <li key={commit.sha}>
                    <CommitItem
                      commit={commit}
                      onViewDiff={onViewDiff}
                      onRollback={
                        canRollback ? (sha) => setRollbackSha(sha) : undefined
                      }
                    />
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      )}

      {/* Rollback confirm */}
      <ConfirmDialog
        open={rollbackSha !== null}
        onOpenChange={(open) => {
          if (!open) setRollbackSha(null);
        }}
        title={t("rollbackConfirmTitle")}
        description={t("rollbackConfirmBody")}
        confirmLabel={t("rollback")}
        onConfirm={confirmRollback}
        loading={rollback.isPending}
      />
    </section>
  );
}
