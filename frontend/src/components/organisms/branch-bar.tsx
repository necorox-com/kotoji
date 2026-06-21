"use client";

/**
 * BranchBar (organism) — design.md §3.3 / §3.5 ProjectDetail.
 *
 * The sticky strip above the editor / on the Branches tab. It composes:
 *  - BranchSelect (molecule)         — switch the active "version" (branch),
 *  - CopyableUrl (molecule)          — the preview URL of the current branch,
 *  - a create-version dialog         — create a host-safe branch from a source,
 *  - a delete-version action         — guarded (never published/draft),
 *  - PublishStatusPill               — the at-a-glance publish state,
 *  - a quick "公開する / Publish" button that defers to the parent (the real
 *    publish flow lives in PublishPanel; here it just navigates/opens it).
 *
 * Data comes from the branch hooks (useBranches / useCreateBranch /
 * useDeleteBranch); role gating (who may create/delete/publish) is driven by the
 * `role` prop per CANONICAL.md §6.1 (owner/editor write; viewer read-only).
 *
 * Responsive: wraps to multiple rows on phones; the bar is `flex-wrap` so the
 * preview URL and actions stack rather than overflow at 375px.
 */

import { useMemo, useState } from "react";
import { GitBranch, Trash2, Upload } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { BranchSelect } from "@/components/molecules/branch-select";
import { CopyableUrl } from "@/components/molecules/copyable-url";
import { ConfirmDialog } from "@/components/molecules/confirm-dialog";
import { ErrorState } from "@/components/molecules/error-state";
import { LoadingState } from "@/components/molecules/loading-state";
import { Spinner } from "@/components/atoms";
import { StatusBadge } from "@/components/atoms/status-badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useBranches, useCreateBranch, useDeleteBranch } from "@/lib/api/hooks";
import { errorMessage } from "@/lib/api/error";
import { roleCan } from "@/lib/api/capabilities";
import type { Branch, SiteRole } from "@/lib/api/types";
import { cn } from "@/lib/utils";

// Reserved logical branches that can never be deleted (CANONICAL.md §5.2).
const PROTECTED_BRANCHES = new Set(["published", "draft"]);

export interface BranchBarProps {
  handle: string;
  /** The currently selected branch ("version"). */
  branch: string;
  /** Switch the active branch (parent owns the URL/state). */
  onBranchChange: (branch: string) => void;
  /** The caller's role on this site (drives create/delete/publish gating). */
  role: SiteRole;
  /** Base hosting domain for composing preview URLs (e.g. hosting.example.com). */
  baseDomain: string;
  /** Open the publish flow (PublishPanel owns the real mutation). */
  onPublish?: () => void;
  /** Plain-language publish state shown by the pill. */
  publishMode?: "direct" | "request";
  className?: string;
}

/** Compose a full preview URL from a branch's host label fragment. */
function previewUrl(branch: Branch, baseDomain: string): string {
  // Branch.previewSubdomain is "{handle}" (published) or "{handle}--{branch}".
  return `${branch.previewSubdomain}.${baseDomain}`;
}

export function BranchBar({
  handle,
  branch,
  onBranchChange,
  role,
  baseDomain,
  onPublish,
  publishMode = "direct",
  className,
}: BranchBarProps) {
  const t = useTranslations("branches");
  const tp = useTranslations("publish");
  const tc = useTranslations("common");

  const branchesQuery = useBranches(handle);
  const createBranch = useCreateBranch(handle);
  const deleteBranch = useDeleteBranch(handle);

  const [createOpen, setCreateOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [newName, setNewName] = useState("");
  const [fromBranch, setFromBranch] = useState("draft");

  const canWrite = roleCan(role, "write");
  const canPublish = roleCan(role, "publish");

  // Stabilize the derived array so the `current` memo's deps don't change every
  // render (a fresh `?? []` literal would otherwise be a new reference).
  const branches = useMemo(
    () => branchesQuery.data ?? [],
    [branchesQuery.data],
  );

  // The Branch object for the active selection (for its preview URL + protection).
  const current = useMemo(
    () => branches.find((b) => b.name === branch),
    [branches, branch],
  );

  const isProtected = PROTECTED_BRANCHES.has(branch);

  const openCreate = () => {
    setNewName("");
    // Default the source to the current branch so "branch from here" is natural.
    setFromBranch(branch || "draft");
    setCreateOpen(true);
  };

  const submitCreate = async () => {
    const name = newName.trim();
    if (!name) return;
    try {
      const created = await createBranch.mutateAsync({
        name,
        from: fromBranch,
      });
      toast.success(t("create"));
      setCreateOpen(false);
      // Switch to the freshly created version.
      onBranchChange(created.name);
    } catch (err) {
      toast.error(errorMessage(err, t("loadError")));
    }
  };

  const submitDelete = async () => {
    try {
      await deleteBranch.mutateAsync({ branch });
      toast.success(t("delete"));
      setDeleteOpen(false);
      // Fall back to draft after deleting the active version.
      onBranchChange("draft");
    } catch (err) {
      toast.error(errorMessage(err, t("loadError")));
    }
  };

  if (branchesQuery.isLoading) {
    return (
      <div
        className={cn("border-b border-border bg-card px-4 py-2", className)}
        data-slot="branch-bar"
      >
        <LoadingState label={t("title")} className="py-3" />
      </div>
    );
  }

  if (branchesQuery.isError) {
    return (
      <div
        className={cn("border-b border-border bg-card px-4 py-2", className)}
        data-slot="branch-bar"
      >
        <ErrorState
          error={branchesQuery.error}
          title={t("loadError")}
          onRetry={() => branchesQuery.refetch()}
        />
      </div>
    );
  }

  // Publish-state pill: "公開中" when on/after published, otherwise "下書きが先行".
  const publishStatus = current?.isPublished ? "published" : "draft";

  return (
    <div
      data-slot="branch-bar"
      className={cn(
        "sticky top-0 z-10 flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-border bg-card px-4 py-2",
        className,
      )}
    >
      {/* Version picker */}
      <div className="flex items-center gap-2">
        <GitBranch
          className="size-4 shrink-0 text-muted-foreground"
          aria-hidden="true"
        />
        <BranchSelect
          branches={branches}
          value={branch}
          onValueChange={onBranchChange}
          onCreateNew={canWrite ? openCreate : undefined}
          aria-label={t("current")}
        />
      </div>

      {/* Preview URL of the active version */}
      {current ? (
        <div className="flex min-w-0 items-center gap-1.5">
          <span className="shrink-0 text-xs text-muted-foreground">
            {t("preview")}
          </span>
          <CopyableUrl
            value={previewUrl(current, baseDomain)}
            className="min-w-0 max-w-[18rem]"
          />
        </div>
      ) : null}

      {/* Status pill — at-a-glance publish state */}
      <StatusBadge status={publishStatus} />

      {/* Spacer pushes actions to the trailing edge on wide rows */}
      <div className="ml-auto flex items-center gap-2">
        {/* Delete this version (owner/editor; never published/draft) */}
        {canWrite && !isProtected ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setDeleteOpen(true)}
            aria-label={t("delete")}
          >
            <Trash2 aria-hidden="true" />
            <span className="hidden sm:inline">{t("delete")}</span>
          </Button>
        ) : null}

        {/* Quick publish — defers to PublishPanel; copy depends on publish_mode */}
        {canPublish && onPublish ? (
          <Button
            // Gold-ringed primary for the reserved publish CTA (design.md §3.1
            // "publish" variant intent; the base Button has no publish variant
            // so we apply the gold ring inline to keep it visually unique).
            className="ring-1 ring-inset ring-brand-gold/40"
            size="sm"
            onClick={onPublish}
          >
            <Upload aria-hidden="true" />
            {publishMode === "request" ? tp("requestPublish") : tp("publish")}
          </Button>
        ) : null}
      </div>

      {/* Create-version dialog */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("newVersion")}</DialogTitle>
            <DialogDescription>{t("empty.body")}</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4">
            <div className="grid gap-2">
              <Label htmlFor="branch-bar-new-name">{t("newVersionName")}</Label>
              <Input
                id="branch-bar-new-name"
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="feature-me-idea"
                className="font-mono"
                autoComplete="off"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="branch-bar-from">{t("from")}</Label>
              <Select
                value={fromBranch}
                onValueChange={(v) => v != null && setFromBranch(v)}
              >
                <SelectTrigger
                  id="branch-bar-from"
                  aria-label={t("from")}
                  className="w-full"
                >
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
            </div>
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setCreateOpen(false)}
              disabled={createBranch.isPending}
            >
              {tc("cancel")}
            </Button>
            <Button
              onClick={submitCreate}
              disabled={createBranch.isPending || newName.trim().length === 0}
              aria-busy={createBranch.isPending}
            >
              {createBranch.isPending ? <Spinner size="sm" /> : null}
              {t("create")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Delete-version confirm */}
      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        variant="destructive"
        title={t("deleteConfirmTitle")}
        description={t("deleteConfirmBody", { name: branch })}
        confirmLabel={t("delete")}
        onConfirm={submitDelete}
        loading={deleteBranch.isPending}
      />
    </div>
  );
}
