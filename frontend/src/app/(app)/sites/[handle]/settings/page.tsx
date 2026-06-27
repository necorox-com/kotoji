"use client";

/**
 * ProjectDetail · Settings — design.md §3.5 (Settings: rename/delete + members +
 * GitHub) / §3.3 SiteSettingsForm.
 *
 * Composes the settings surface (no SiteSettingsForm organism is exported in this
 * codebase, so it's built here from primitives + the site hooks per the design
 * inventory):
 *  - General: visibility / publish mode / description (useUpdateSite),
 *  - Rename handle (useRenameSite) with the plain-language redirect note,
 *  - GitHub mirror linking + manual sync (GitHubSection),
 *  - Danger zone: delete the site with a typed-handle confirmation (useDeleteSite).
 *
 * NOTE: MCP/API tokens are NO LONGER issued here. Under the re-architected model
 * (CANONICAL §6) a token is owned by the USER (not a project) and automatically
 * covers every project they're a member of, so token management lives on the
 * account /settings page (AccountTokenPanel), not on a project.
 *
 * Role gating (CANONICAL §6.1): all of the above are owner-only ("manageSettings"
 * / "rename" / "deleteSite"). Non-owners see a read-only note. The loading/error
 * triplet covers the site detail fetch.
 */

import { use, useState } from "react";
import { useRouter } from "next/navigation";
import { useTranslations } from "next-intl";
import { toast } from "sonner";
import { Trash2 } from "lucide-react";

import { GitHubSection } from "@/components/organisms";
import { ConfirmDialog } from "@/components/molecules/confirm-dialog";
import { ErrorState } from "@/components/molecules/error-state";
import { LoadingState } from "@/components/molecules/loading-state";
import { SectionHeading, CodeText } from "@/components/atoms";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  useSite,
  useConfig,
  useUpdateSite,
  useRenameSite,
  useDeleteSite,
} from "@/lib/api/hooks";
import { errorMessage } from "@/lib/api/error";
import { roleCan } from "@/lib/api/capabilities";
import { useSiteRole } from "@/hooks";
import type {
  PublishMode,
  SiteVisibility,
} from "@/lib/api/types";

const VISIBILITY_VALUES: SiteVisibility[] = ["private", "members", "public"];
const PUBLISH_MODES: PublishMode[] = ["direct", "request"];
const DEFAULT_BASE_DOMAIN = "hosting.example.com";
const HANDLE_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

// Local route-params type (Next async `params`); see page.tsx for rationale.
type SiteParams = { params: Promise<{ handle: string }> };

export default function SettingsPage({ params }: SiteParams) {
  const { handle } = use(params);
  const t = useTranslations("settings");
  const tc = useTranslations("common");
  const tAuth = useTranslations("auth");
  const tCreate = useTranslations("createSite");
  const router = useRouter();

  const siteQuery = useSite(handle);
  const { data: config } = useConfig();
  const { role } = useSiteRole(handle);

  const updateSite = useUpdateSite(handle);
  const renameSite = useRenameSite(handle);
  const deleteSite = useDeleteSite();

  const baseDomain = config?.baseDomain ?? DEFAULT_BASE_DOMAIN;
  const canManage = roleCan(role, "manageSettings");
  const canRename = roleCan(role, "rename");
  const canDelete = roleCan(role, "deleteSite");

  // General settings local form state (seeded from the fetched site).
  const [visibility, setVisibility] = useState<SiteVisibility>("private");
  const [publishMode, setPublishMode] = useState<PublishMode>("direct");
  const [description, setDescription] = useState("");
  // Rename + delete local state.
  const [newHandle, setNewHandle] = useState("");
  const [deleteOpen, setDeleteOpen] = useState(false);

  // Seed the form from the site DURING render (React-recommended over a
  // set-state-in-effect; mirrors MonacoEditorPanel's seededKey idiom). We re-seed
  // whenever the site identity changes (id + updatedAt), e.g. after a save.
  const site = siteQuery.data;
  const [seededKey, setSeededKey] = useState<string>("");
  if (site) {
    const key = `${site.id}@${site.updatedAt}`;
    if (key !== seededKey) {
      setSeededKey(key);
      setVisibility(site.visibility);
      setPublishMode(site.publishMode);
      setDescription(site.description ?? "");
      setNewHandle(site.handle);
    }
  }

  if (siteQuery.isLoading) {
    return <LoadingState rows={4} label={t("title")} />;
  }
  if (siteQuery.isError || !site) {
    return (
      <ErrorState
        error={siteQuery.error}
        title={t("title")}
        onRetry={() => siteQuery.refetch()}
      />
    );
  }

  const saveGeneral = async () => {
    try {
      await updateSite.mutateAsync({ visibility, publishMode, description });
      toast.success(t("saved"));
    } catch (err) {
      toast.error(errorMessage(err, t("saveError")));
    }
  };

  const submitRename = async () => {
    const next = newHandle.trim().toLowerCase();
    if (!next || next === site.handle) return;
    try {
      const renamed = await renameSite.mutateAsync({ newHandle: next });
      toast.success(t("renamed"));
      // The handle changed → the URL must follow.
      router.replace(`/sites/${renamed.handle}/settings`);
    } catch (err) {
      toast.error(errorMessage(err, t("renameError")));
    }
  };

  const confirmDelete = async () => {
    try {
      await deleteSite.mutateAsync({ handle: site.handle });
      toast.success(t("deleted"));
      setDeleteOpen(false);
      router.replace("/dashboard");
    } catch (err) {
      toast.error(errorMessage(err, t("deleteError")));
    }
  };

  const renameValid =
    newHandle.length > 0 &&
    newHandle !== site.handle &&
    HANDLE_RE.test(newHandle) &&
    !newHandle.includes("--");

  return (
    <div className="space-y-10">
      <SectionHeading level={1} title={t("title")} />

      {/* Read-only note for non-owners. */}
      {!canManage ? (
        <p className="rounded-lg border border-border bg-muted/40 px-3 py-2 text-sm text-muted-foreground">
          {tAuth("notAuthorized")}
        </p>
      ) : null}

      {/* General settings */}
      <section className="space-y-4" aria-labelledby="settings-general">
        <h2
          id="settings-general"
          className="text-lg font-semibold text-foreground"
        >
          {t("title")}
        </h2>

        <div className="grid gap-2">
          <Label htmlFor="settings-visibility">{t("visibility")}</Label>
          <Select
            value={visibility}
            onValueChange={(v) => v != null && setVisibility(v as SiteVisibility)}
            disabled={!canManage}
          >
            <SelectTrigger
              id="settings-visibility"
              className="w-full sm:max-w-xs"
              aria-label={t("visibility")}
            >
              <SelectValue>
                {(v: SiteVisibility) =>
                  t(
                    v === "public"
                      ? "visibilityPublic"
                      : v === "members"
                        ? "visibilityMembers"
                        : "visibilityPrivate"
                  )
                }
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              {VISIBILITY_VALUES.map((v) => (
                <SelectItem key={v} value={v}>
                  {t(
                    v === "public"
                      ? "visibilityPublic"
                      : v === "members"
                        ? "visibilityMembers"
                        : "visibilityPrivate"
                  )}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        <div className="grid gap-2">
          <Label htmlFor="settings-publish-mode">{t("publishMode")}</Label>
          <Select
            value={publishMode}
            onValueChange={(v) => v != null && setPublishMode(v as PublishMode)}
            disabled={!canManage}
          >
            <SelectTrigger
              id="settings-publish-mode"
              className="w-full sm:max-w-xs"
              aria-label={t("publishMode")}
            >
              <SelectValue>
                {(v: PublishMode) =>
                  t(v === "request" ? "publishModeRequest" : "publishModeDirect")
                }
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              {PUBLISH_MODES.map((m) => (
                <SelectItem key={m} value={m}>
                  {t(m === "request" ? "publishModeRequest" : "publishModeDirect")}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        <div className="grid gap-2">
          <Label htmlFor="settings-description">{t("description")}</Label>
          <Textarea
            id="settings-description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            disabled={!canManage}
          />
        </div>

        {canManage ? (
          <div className="flex justify-end">
            <Button
              onClick={saveGeneral}
              disabled={updateSite.isPending}
              aria-busy={updateSite.isPending}
            >
              {updateSite.isPending ? tc("saving") : tc("save")}
            </Button>
          </div>
        ) : null}
      </section>

      {/* Rename handle (owner only) */}
      {canRename ? (
        <section className="space-y-3" aria-labelledby="settings-rename">
          <h2
            id="settings-rename"
            className="text-lg font-semibold text-foreground"
          >
            {t("renameHandle")}
          </h2>
          <p className="text-sm text-muted-foreground">{t("renameNote")}</p>
          <div className="flex flex-col gap-2 sm:flex-row sm:items-end">
            <div className="grid flex-1 gap-1.5">
              <Label htmlFor="settings-handle">{tCreate("handleLabel")}</Label>
              <Input
                id="settings-handle"
                value={newHandle}
                onChange={(e) => setNewHandle(e.target.value.toLowerCase())}
                className="font-mono"
                autoComplete="off"
                aria-describedby="settings-handle-url"
              />
            </div>
            <Button
              onClick={submitRename}
              disabled={!renameValid || renameSite.isPending}
              aria-busy={renameSite.isPending}
            >
              {renameSite.isPending ? tc("saving") : tc("rename")}
            </Button>
          </div>
          <p id="settings-handle-url" className="text-sm text-muted-foreground">
            {tCreate("urlPreview")}:{" "}
            <CodeText>{`https://${newHandle || site.handle}.${baseDomain}`}</CodeText>
          </p>
        </section>
      ) : null}

      {/* GitHub mirror linking + manual sync (owner-only; renders null otherwise). */}
      <GitHubSection
        handle={handle}
        role={role}
        githubRepo={site.githubRepo ?? null}
        mirrorEnabled={config?.githubMirrorEnabled ?? false}
      />

      {/* AI / MCP tokens are now PER-USER (CANONICAL §6): they're issued from
          your account Settings (/settings), not per project — one token spans
          every project you're a member of. So no token panel here. */}

      {/* Danger zone (owner only) */}
      {canDelete ? (
        <section
          className="space-y-3 rounded-lg border border-destructive/40 bg-destructive/5 p-4"
          aria-labelledby="settings-danger"
        >
          <h2
            id="settings-danger"
            className="text-lg font-semibold text-destructive"
          >
            {t("dangerZone")}
          </h2>
          <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-sm text-muted-foreground">
              {t("deleteConfirmBody", { handle: site.handle })}
            </p>
            <Button
              variant="destructive"
              onClick={() => setDeleteOpen(true)}
              className="shrink-0"
            >
              <Trash2 aria-hidden="true" />
              {t("deleteSite")}
            </Button>
          </div>
        </section>
      ) : null}

      {/* Typed-handle delete confirmation. */}
      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        variant="destructive"
        title={t("deleteConfirmTitle")}
        description={t("deleteConfirmBody", { handle: site.handle })}
        confirmPhrase={site.handle}
        confirmPhraseLabel={t("deleteTypeHandle", { handle: site.handle })}
        confirmLabel={t("deleteSite")}
        onConfirm={confirmDelete}
        loading={deleteSite.isPending}
      />
    </div>
  );
}
