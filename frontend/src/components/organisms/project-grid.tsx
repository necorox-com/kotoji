"use client";

/**
 * ProjectGrid (organism) — design.md §3.3 / §3.5 (Dashboard). The site list:
 * a responsive grid of ProjectCard (1/2/3/4 cols at base/sm/lg/2xl), a debounced
 * SearchBar, owner/all + status filter chips, and the mandatory triplet —
 * loading (skeleton cards), error (ErrorState + retry), empty (EmptyState + CTA).
 *
 * Data comes from useSites() (the list payload) and useConfig() (baseDomain for
 * the ProjectCard preview URLs). Filtering/searching is client-side over the
 * already-fetched list (the list is small; design.md §4.2 keeps it on one query).
 */

import { useMemo, useState } from "react";
import NextLink from "next/link";
import { useTranslations } from "next-intl";
import { Plus } from "lucide-react";
import { SearchBar, EmptyState, ErrorState } from "@/components/molecules";
import { ProjectCard } from "@/components/molecules";
import { Chip } from "@/components/atoms";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useSites, useConfig } from "@/lib/api/hooks";
import type { SiteSummary } from "@/lib/api/types";
import { cn } from "@/lib/utils";

/** "Mine" = sites where the caller is the owner (CANONICAL §6 role axis). */
type OwnerFilter = "all" | "mine";
/** Status filter over the derived published/draft state. */
type StatusFilter = "all" | "published" | "draft";

/** Skeleton card mirroring ProjectCard's footprint during load (§4.2). */
function CardSkeleton() {
  return (
    <div className="flex flex-col gap-3 rounded-lg border border-border bg-card p-6">
      <div className="flex items-center justify-between gap-2">
        <Skeleton className="h-5 w-32" />
        <Skeleton className="h-5 w-16 rounded-sm" />
      </div>
      <Skeleton className="h-4 w-full" />
      <Skeleton className="h-8 w-full rounded-md" />
    </div>
  );
}

const GRID_CLASS =
  "grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4";

export interface ProjectGridProps {
  className?: string;
}

export function ProjectGrid({ className }: ProjectGridProps) {
  const t = useTranslations();
  const sitesQuery = useSites();
  const { data: config } = useConfig();

  const [search, setSearch] = useState("");
  const [owner, setOwner] = useState<OwnerFilter>("all");
  const [status, setStatus] = useState<StatusFilter>("all");

  const { data: sites, isLoading, isError, error, refetch } = sitesQuery;

  // Apply owner + status + text filters in one memoized pass.
  const filtered = useMemo<SiteSummary[]>(() => {
    const term = search.trim().toLowerCase();
    return (sites ?? []).filter((site) => {
      if (owner === "mine" && site.role !== "owner") return false;
      if (status === "published" && !site.hasPublished) return false;
      if (status === "draft" && site.hasPublished) return false;
      if (term) {
        const haystack = `${site.handle} ${site.description ?? ""}`.toLowerCase();
        if (!haystack.includes(term)) return false;
      }
      return true;
    });
  }, [sites, owner, status, search]);

  const newSiteButton = (
    <Button render={<NextLink href="/sites/new" />}>
      <Plus aria-hidden="true" />
      {t("dashboard.newSite")}
    </Button>
  );

  return (
    <div className={cn("space-y-6", className)}>
      {/* Controls: search + filter chips. Stacks on phone, row on ≥sm. */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
        <SearchBar
          onDebouncedChange={setSearch}
          placeholder={t("common.search")}
          className="sm:max-w-xs"
        />
        <div className="flex flex-wrap items-center gap-2">
          <Chip
            active={owner === "all"}
            onClick={() => setOwner("all")}
            role="button"
            tabIndex={0}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                setOwner("all");
              }
            }}
            aria-pressed={owner === "all"}
            className="cursor-pointer"
          >
            {t("dashboard.filterAll")}
          </Chip>
          <Chip
            active={owner === "mine"}
            onClick={() => setOwner("mine")}
            role="button"
            tabIndex={0}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                setOwner("mine");
              }
            }}
            aria-pressed={owner === "mine"}
            className="cursor-pointer"
          >
            {t("dashboard.filterMine")}
          </Chip>
          <span className="mx-1 h-4 w-px bg-border" aria-hidden="true" />
          <Chip
            active={status === "published"}
            onClick={() =>
              setStatus((s) => (s === "published" ? "all" : "published"))
            }
            role="button"
            tabIndex={0}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                setStatus((s) => (s === "published" ? "all" : "published"));
              }
            }}
            aria-pressed={status === "published"}
            className="cursor-pointer"
          >
            {t("dashboard.filterPublished")}
          </Chip>
          <Chip
            active={status === "draft"}
            onClick={() => setStatus((s) => (s === "draft" ? "all" : "draft"))}
            role="button"
            tabIndex={0}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                setStatus((s) => (s === "draft" ? "all" : "draft"));
              }
            }}
            aria-pressed={status === "draft"}
            className="cursor-pointer"
          >
            {t("dashboard.filterDraft")}
          </Chip>
        </div>
      </div>

      {/* Triplet: loading / error / (empty | content). */}
      {isLoading ? (
        <div className={GRID_CLASS} aria-busy="true">
          {Array.from({ length: 6 }).map((_, i) => (
            <CardSkeleton key={i} />
          ))}
        </div>
      ) : isError ? (
        <ErrorState
          error={error}
          title={t("dashboard.loadError")}
          onRetry={() => refetch()}
        />
      ) : filtered.length === 0 ? (
        // Distinguish "no sites at all" from "no matches for the active filter".
        (sites?.length ?? 0) === 0 ? (
          <EmptyState
            title={t("dashboard.empty.title")}
            body={t("dashboard.empty.body")}
            action={newSiteButton}
          />
        ) : (
          <EmptyState title={t("errors.notFound")} />
        )
      ) : (
        <div className={GRID_CLASS}>
          {filtered.map((site) => (
            <ProjectCard
              key={site.id}
              site={site}
              baseDomain={config?.baseDomain}
            />
          ))}
        </div>
      )}
    </div>
  );
}
