"use client";

/**
 * ProjectDetail · History — design.md §3.5 ProjectDetail · History.
 *
 * HistoryTimeline owns the log list, the version/source filters, and rollback
 * (behind a confirm). This page wires the caller's role (rollback is owner/editor
 * only) and opens a commit's diff in a dialog: `onViewDiff(sha)` → DiffViewer in
 * refs mode comparing the commit (`{sha}`) against its parent (`{sha}^`), which
 * is the natural "what changed in this version" view (design.md §3.5: "Diff opens
 * DiffViewer (side-by-side desktop, unified phone)").
 */

import { use, useState } from "react";
import { useSearchParams } from "next/navigation";
import { useTranslations } from "next-intl";
import { HistoryTimeline, DiffViewer } from "@/components/organisms";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { useSiteRole } from "@/hooks";

const DEFAULT_BRANCH = "draft";

// Local route-params type (Next async `params`); see page.tsx for rationale.
type SiteParams = { params: Promise<{ handle: string }> };

export default function HistoryPage({ params }: SiteParams) {
  const { handle } = use(params);
  const t = useTranslations("history");
  const searchParams = useSearchParams();

  const { role } = useSiteRole(handle);

  // HistoryTimeline owns the branch filter selection internally, but the initial
  // value follows the shared `?branch=` selection. We keep our own state so the
  // dialog + timeline agree on which version's history is shown.
  const [branch, setBranch] = useState(
    () => searchParams.get("branch") ?? DEFAULT_BRANCH
  );
  const [diffSha, setDiffSha] = useState<string | null>(null);

  return (
    <>
      <HistoryTimeline
        handle={handle}
        branch={branch}
        onBranchChange={setBranch}
        role={role}
        onViewDiff={(sha) => setDiffSha(sha)}
      />

      <Dialog
        open={diffSha !== null}
        onOpenChange={(open) => {
          if (!open) setDiffSha(null);
        }}
      >
        <DialogContent className="max-h-[90dvh] overflow-hidden sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>{t("viewDiff")}</DialogTitle>
          </DialogHeader>
          {diffSha ? (
            <DiffViewer
              mode="refs"
              handle={handle}
              from={`${diffSha}^`}
              to={diffSha}
              path=""
              height={480}
            />
          ) : null}
        </DialogContent>
      </Dialog>
    </>
  );
}
