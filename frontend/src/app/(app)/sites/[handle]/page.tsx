"use client";

/**
 * ProjectDetail · Files/Editor (default section) — design.md §3.4.3 / §3.5.
 *
 * The split-pane editor. Responsive collapse (design.md §3.4.3):
 *  - desktop (`lg:`): a resizable 2-pane — FileTree (left, 180–320px, default
 *    240) | MonacoEditorPanel (center, flex-1) — via react-resizable-panels.
 *  - tablet (`md` 768–1024): the split collapses to a TabBar (Tree | Editor)
 *    inside the Files section so the editor stays usable without a cramped tree.
 *  - phone (<640): no inline tree; the tree opens in a Sheet drawer (the
 *    MobileFileDrawer pattern), and MonacoEditorPanel renders its own read-only
 *    viewer + "ask AI / edit elsewhere" affordance (principle #6).
 *
 * Conflict: when a save returns a 409 (stale baseSha) MonacoEditorPanel raises
 * `onConflict`; we render ConflictResolver in a dialog. "Reload" re-seeds the
 * editor (we bump a key to force a fresh open); "overwrite" resolves server-side.
 *
 * Active branch is read from `?branch=` (the layout's BranchBar owns it), default
 * "draft". The dirty set drives the FileTree dirty dots.
 */

import { use, useCallback, useState } from "react";
import { useSearchParams } from "next/navigation";
import { useTranslations } from "next-intl";
import { Files as FilesIcon, FolderTree } from "lucide-react";
import {
  FileTree,
  MonacoEditorPanel,
  ConflictResolver,
} from "@/components/organisms";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
} from "@/components/ui/resizable";
import { Button } from "@/components/ui/button";
import type { ConflictError } from "@/lib/api/error";

const DEFAULT_BRANCH = "draft";

// Local route-params type: matches Next's async `params` (a Promise) for this
// dynamic segment. Declared explicitly so `tsc --noEmit` is self-contained and
// doesn't depend on Next's generated `.next/types` globals (PageProps), which
// only exist after `next dev`/`next build`/`next typegen`.
type SiteParams = { params: Promise<{ handle: string }> };

export default function FilesPage({ params }: SiteParams) {
  // params is a Promise in this Next version; unwrap with React `use`.
  const { handle } = use(params);
  const t = useTranslations("files");
  const tEditor = useTranslations("editor");

  const searchParams = useSearchParams();
  const branch = searchParams.get("branch") ?? DEFAULT_BRANCH;

  const [selectedPath, setSelectedPath] = useState<string | undefined>(
    undefined
  );
  const [dirtyPaths, setDirtyPaths] = useState<Set<string>>(new Set());
  const [drawerOpen, setDrawerOpen] = useState(false);
  // Bumped on conflict-reload to force MonacoEditorPanel to re-open the file
  // fresh (re-seeding the baseSha from the live tip).
  const [reopenKey, setReopenKey] = useState(0);
  const [conflict, setConflict] = useState<{
    error: ConflictError;
    path: string;
    attempted: string;
  } | null>(null);

  // Track per-file dirty state for the FileTree dirty dots.
  const onDirtyChange = useCallback(
    (dirty: boolean) => {
      setDirtyPaths((prev) => {
        if (!selectedPath) return prev;
        const has = prev.has(selectedPath);
        if (dirty === has) return prev; // no change
        const next = new Set(prev);
        if (dirty) next.add(selectedPath);
        else next.delete(selectedPath);
        return next;
      });
    },
    [selectedPath]
  );

  const onConflict = useCallback(
    (error: ConflictError, attemptedContent: string) => {
      if (!selectedPath) return;
      setConflict({ error, path: selectedPath, attempted: attemptedContent });
    },
    [selectedPath]
  );

  // Reload to server version: drop dirty + re-open fresh (new baseSha).
  const onReloadServer = useCallback(() => {
    setConflict(null);
    setDirtyPaths((prev) => {
      if (!conflict) return prev;
      const next = new Set(prev);
      next.delete(conflict.path);
      return next;
    });
    setReopenKey((k) => k + 1);
  }, [conflict]);

  const onResolved = useCallback(() => {
    setConflict(null);
    setDirtyPaths((prev) => {
      if (!conflict) return prev;
      const next = new Set(prev);
      next.delete(conflict.path);
      return next;
    });
    setReopenKey((k) => k + 1);
  }, [conflict]);

  const selectFromTree = useCallback((path: string) => {
    setSelectedPath(path);
    setDrawerOpen(false);
  }, []);

  // The editor panel (shared across breakpoints; it self-adapts to phone).
  const editor = (
    <MonacoEditorPanel
      key={`${branch}:${selectedPath ?? ""}:${reopenKey}`}
      handle={handle}
      branch={branch}
      path={selectedPath}
      onDirtyChange={onDirtyChange}
      onConflict={onConflict}
      className="h-full"
    />
  );

  const tree = (
    <FileTree
      handle={handle}
      branch={branch}
      selectedPath={selectedPath}
      onSelect={selectFromTree}
      dirtyPaths={dirtyPaths}
      className="h-full"
    />
  );

  return (
    <div className="flex h-[calc(100dvh-7.5rem)] min-h-0 flex-col">
      {/* ---- Desktop (lg+): resizable split-pane ---- */}
      <div className="hidden min-h-0 flex-1 lg:block">
        <ResizablePanelGroup orientation="horizontal" className="h-full">
          <ResizablePanel
            defaultSize="22%"
            minSize="14%"
            maxSize="36%"
            className="border-r border-border bg-card"
          >
            {tree}
          </ResizablePanel>
          <ResizableHandle withHandle />
          <ResizablePanel defaultSize="78%" className="min-w-0">
            {editor}
          </ResizablePanel>
        </ResizablePanelGroup>
      </div>

      {/* ---- Tablet (md–lg): Tree | Editor tabs ---- */}
      <div className="hidden min-h-0 flex-1 md:flex md:flex-col lg:hidden">
        <Tabs defaultValue="editor" className="flex h-full min-h-0 flex-col">
          <TabsList className="self-start">
            <TabsTrigger value="tree">
              <FolderTree aria-hidden="true" />
              {t("title")}
            </TabsTrigger>
            <TabsTrigger value="editor">
              <FilesIcon aria-hidden="true" />
              {tEditor("save")}
            </TabsTrigger>
          </TabsList>
          <TabsContent
            value="tree"
            className="min-h-0 flex-1 rounded-lg border border-border bg-card"
          >
            {tree}
          </TabsContent>
          <TabsContent value="editor" className="min-h-0 flex-1">
            {editor}
          </TabsContent>
        </Tabs>
      </div>

      {/* ---- Phone (<md): drawer for the tree + read-only editor ---- */}
      <div className="flex min-h-0 flex-1 flex-col md:hidden">
        <div className="flex shrink-0 items-center gap-2 px-3 py-2">
          <Sheet open={drawerOpen} onOpenChange={setDrawerOpen}>
            <SheetTrigger
              render={
                <Button variant="outline" size="sm">
                  <FolderTree aria-hidden="true" />
                  {t("title")}
                </Button>
              }
            />
            <SheetContent side="left" className="w-80 p-0">
              <SheetHeader className="border-b border-border">
                <SheetTitle>{t("title")}</SheetTitle>
              </SheetHeader>
              <div className="h-[calc(100dvh-4rem)]">{tree}</div>
            </SheetContent>
          </Sheet>
        </div>
        <div className="min-h-0 flex-1">{editor}</div>
      </div>

      {/* ---- Save conflict (optimistic lock) ---- */}
      <Dialog
        open={conflict !== null}
        onOpenChange={(open) => {
          if (!open) setConflict(null);
        }}
      >
        <DialogContent className="max-h-[90dvh] overflow-auto sm:max-w-3xl">
          <DialogHeader>
            <DialogTitle>{tEditor("unsavedChanges")}</DialogTitle>
          </DialogHeader>
          {conflict ? (
            <ConflictResolver
              handle={handle}
              branch={branch}
              path={conflict.path}
              conflict={conflict.error}
              attemptedContent={conflict.attempted}
              onReload={onReloadServer}
              onResolved={onResolved}
              onCancel={() => setConflict(null)}
            />
          ) : null}
        </DialogContent>
      </Dialog>
    </div>
  );
}
