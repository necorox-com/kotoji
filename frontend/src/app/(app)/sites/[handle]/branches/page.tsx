"use client";

/**
 * ProjectDetail · Branches ("versions") — design.md §3.5.
 *
 * Version management lives in the BranchBar (rendered in the ProjectDetailLayout
 * chrome above: switch / create / delete / preview-URL / quick-publish). This
 * section page presents the full list of versions with their preview URLs and
 * publish state so a non-engineer can see every "alternate version" at a glance,
 * framed as versions (not git branches) per design.md §1.2 #3.
 */

import { use } from "react";
import { useSearchParams } from "next/navigation";
import { useTranslations } from "next-intl";
import { GitBranch } from "lucide-react";
import { CopyableUrl } from "@/components/molecules/copyable-url";
import { EmptyState } from "@/components/molecules/empty-state";
import { ErrorState } from "@/components/molecules/error-state";
import { LoadingState } from "@/components/molecules/loading-state";
import { StatusBadge } from "@/components/atoms";
import { useBranches, useConfig } from "@/lib/api/hooks";

const DEFAULT_BASE_DOMAIN = "hosting.example.com";

// Local route-params type (Next async `params`); see page.tsx for rationale.
type SiteParams = { params: Promise<{ handle: string }> };

export default function BranchesPage({ params }: SiteParams) {
  const { handle } = use(params);
  const t = useTranslations("branches");
  const searchParams = useSearchParams();
  const activeBranch = searchParams.get("branch") ?? "draft";

  const branchesQuery = useBranches(handle);
  const { data: config } = useConfig();
  const baseDomain = config?.baseDomain ?? DEFAULT_BASE_DOMAIN;

  return (
    <section className="space-y-4" aria-labelledby="branches-heading">
      <h1
        id="branches-heading"
        className="text-2xl font-semibold text-foreground"
      >
        {t("title")}
      </h1>

      {branchesQuery.isLoading ? (
        <LoadingState rows={3} label={t("title")} />
      ) : branchesQuery.isError ? (
        <ErrorState
          error={branchesQuery.error}
          title={t("loadError")}
          onRetry={() => branchesQuery.refetch()}
        />
      ) : (branchesQuery.data?.length ?? 0) === 0 ? (
        <EmptyState
          icon={GitBranch}
          title={t("empty.title")}
          body={t("empty.body")}
        />
      ) : (
        <ul className="divide-y divide-border rounded-lg border border-border">
          {branchesQuery.data?.map((b) => (
            <li
              key={b.name}
              className="flex flex-col gap-2 px-4 py-3 sm:flex-row sm:items-center sm:justify-between"
            >
              <div className="flex min-w-0 items-center gap-2">
                <GitBranch
                  className="size-4 shrink-0 text-muted-foreground"
                  aria-hidden="true"
                />
                <span className="truncate font-medium text-foreground">
                  {b.name}
                </span>
                {b.name === activeBranch ? (
                  <StatusBadge status="preview" label={t("current")} />
                ) : null}
                <StatusBadge status={b.isPublished ? "published" : "draft"} />
              </div>
              <CopyableUrl
                value={`${b.previewSubdomain}.${baseDomain}`}
                className="min-w-0 max-w-full sm:max-w-xs"
              />
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
