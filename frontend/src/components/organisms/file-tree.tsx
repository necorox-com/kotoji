"use client";

/**
 * FileTree (organism) — design.md §3.3. The left pane of the editor: a recursive
 * tree of FileTreeItem rows built from the flat (recursive) file listing
 * (useFiles). Drives the MonacoEditorPanel via `selectedPath`/`onSelect`.
 *
 * Behavior (design.md §3.3 + §4.8 a11y):
 *  - Builds a nested tree from the flat `FileEntry[]` (recursive listing). Dirs
 *    are inferred both from `isDir` entries AND from path prefixes of files, so a
 *    listing that omits intermediate dir entries still renders folders.
 *  - Keyboard roving-tabindex tree: ↑/↓ move focus across the *visible* rows,
 *    →/← expand/collapse (or step into/out of) a folder, Enter/Space open a file
 *    or toggle a folder, Home/End jump. Exactly one row is tabbable at a time.
 *  - Dirty files (unsaved in the editor) show the right-aligned warning dot.
 *  - loading → skeleton rows; error → ErrorState + retry; empty → EmptyState.
 *
 * Pure data-in/events-out otherwise: it owns only local expand/focus UI state.
 */

import { useCallback, useMemo, useRef, useState } from "react";
import { useTranslations } from "next-intl";
import { FileX2 } from "lucide-react";
import { useFiles } from "@/lib/api/hooks";
import type { FileEntry } from "@/lib/api/types";
import { FileTreeItem } from "@/components/molecules/file-tree-item";
import { EmptyState } from "@/components/molecules/empty-state";
import { ErrorState } from "@/components/molecules/error-state";
import { LoadingState } from "@/components/molecules/loading-state";
import { ScrollArea } from "@/components/ui/scroll-area";
import { cn } from "@/lib/utils";

/** An internal tree node assembled from the flat listing. */
interface TreeNode {
  name: string;
  path: string; // repo-relative, forward slashes
  isDir: boolean;
  children: TreeNode[];
}

/**
 * Assemble a nested tree from the flat (recursive) FileEntry[]. Intermediate
 * directories are synthesized from file path prefixes so the tree is complete
 * even when the listing only returns leaf files.
 */
function buildTree(entries: FileEntry[]): TreeNode[] {
  const root: TreeNode = { name: "", path: "", isDir: true, children: [] };
  // Index nodes by path for O(1) lookup while inserting; avoids re-scanning.
  const byPath = new Map<string, TreeNode>();
  byPath.set("", root);

  const ensureDir = (path: string): TreeNode => {
    const existing = byPath.get(path);
    if (existing) return existing;
    const segments = path.split("/");
    const name = segments[segments.length - 1];
    const parentPath = segments.slice(0, -1).join("/");
    const parent = ensureDir(parentPath); // recurse to create ancestors
    const node: TreeNode = { name, path, isDir: true, children: [] };
    parent.children.push(node);
    byPath.set(path, node);
    return node;
  };

  for (const entry of entries) {
    const segments = entry.path.split("/").filter(Boolean);
    if (segments.length === 0) continue;
    const parentPath = segments.slice(0, -1).join("/");
    const parent = ensureDir(parentPath);
    if (entry.isDir) {
      ensureDir(entry.path);
    } else {
      // A leaf file. Guard against a duplicate if a dir entry already created it.
      if (!byPath.has(entry.path)) {
        const node: TreeNode = {
          name: segments[segments.length - 1],
          path: entry.path,
          isDir: false,
          children: [],
        };
        parent.children.push(node);
        byPath.set(entry.path, node);
      }
    }
  }

  // Stable sort: directories first, then files, each alphabetical (locale-aware).
  const sortNodes = (nodes: TreeNode[]) => {
    nodes.sort((a, b) => {
      if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
      return a.name.localeCompare(b.name, undefined, { numeric: true });
    });
    for (const n of nodes) if (n.isDir) sortNodes(n.children);
  };
  sortNodes(root.children);
  return root.children;
}

/** Flatten the tree into the currently-visible rows (respecting expand state). */
interface VisibleRow {
  node: TreeNode;
  depth: number;
}
function flattenVisible(
  nodes: TreeNode[],
  expanded: Set<string>,
  depth = 0,
  acc: VisibleRow[] = []
): VisibleRow[] {
  for (const node of nodes) {
    acc.push({ node, depth });
    if (node.isDir && expanded.has(node.path)) {
      flattenVisible(node.children, expanded, depth + 1, acc);
    }
  }
  return acc;
}

export interface FileTreeProps {
  handle: string;
  branch: string;
  /** Currently-open file path (drives selection highlight). */
  selectedPath?: string;
  /** Open a file in the editor. */
  onSelect?: (path: string) => void;
  /** Paths with unsaved edits (show the dirty dot). */
  dirtyPaths?: ReadonlySet<string>;
  className?: string;
}

export function FileTree({
  handle,
  branch,
  selectedPath,
  onSelect,
  dirtyPaths,
  className,
}: FileTreeProps) {
  const t = useTranslations("files");
  // The recursive listing is the source of truth for the whole tree.
  const { data, isPending, isError, error, refetch } = useFiles(handle, branch, {
    recursive: true,
  });

  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  // The roving-tabindex focus target (path of the currently tabbable row).
  const [focusedPath, setFocusedPath] = useState<string | undefined>(undefined);
  const listRef = useRef<HTMLDivElement>(null);

  const tree = useMemo(() => buildTree(data ?? []), [data]);
  const rows = useMemo(
    () => flattenVisible(tree, expanded),
    [tree, expanded]
  );

  const toggle = useCallback((path: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  }, []);

  // The effective focus row: explicit focus, else the selected file, else first.
  const activePath =
    focusedPath ?? selectedPath ?? rows[0]?.node.path;

  // Move keyboard focus to the row at `index` (and make it tabbable).
  const focusIndex = useCallback(
    (index: number) => {
      const clamped = Math.max(0, Math.min(index, rows.length - 1));
      const target = rows[clamped];
      if (!target) return;
      setFocusedPath(target.node.path);
      // Move DOM focus to the row so screen readers announce it. The treeitem
      // sets tabIndex via `selected` in the molecule, so we query by path.
      requestAnimationFrame(() => {
        const el = listRef.current?.querySelector<HTMLElement>(
          `[data-path="${CSS.escape(target.node.path)}"]`
        );
        el?.focus();
      });
    },
    [rows]
  );

  // Tree keyboard model (design.md §3.3 / §4.8): arrow nav + expand/collapse.
  const onKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (rows.length === 0) return;
      const currentIndex = rows.findIndex(
        (r) => r.node.path === activePath
      );
      const current = rows[currentIndex];
      switch (e.key) {
        case "ArrowDown":
          e.preventDefault();
          focusIndex(currentIndex + 1);
          break;
        case "ArrowUp":
          e.preventDefault();
          focusIndex(currentIndex - 1);
          break;
        case "Home":
          e.preventDefault();
          focusIndex(0);
          break;
        case "End":
          e.preventDefault();
          focusIndex(rows.length - 1);
          break;
        case "ArrowRight":
          if (current?.node.isDir) {
            e.preventDefault();
            // Collapsed → expand; already expanded → step into first child.
            if (!expanded.has(current.node.path)) {
              toggle(current.node.path);
            } else {
              focusIndex(currentIndex + 1);
            }
          }
          break;
        case "ArrowLeft":
          if (current?.node.isDir && expanded.has(current.node.path)) {
            e.preventDefault();
            toggle(current.node.path);
          } else if (current) {
            // Step out to the parent row.
            e.preventDefault();
            const parentPath = current.node.path
              .split("/")
              .slice(0, -1)
              .join("/");
            const parentIndex = rows.findIndex(
              (r) => r.node.path === parentPath
            );
            if (parentIndex >= 0) focusIndex(parentIndex);
          }
          break;
        default:
          break;
      }
    },
    [rows, activePath, expanded, toggle, focusIndex]
  );

  // --- loading / error / empty triplet (design.md §4.2) ---
  if (isPending) {
    return (
      <div className={cn("p-2", className)}>
        <LoadingState rows={6} label={t("title")} />
      </div>
    );
  }
  if (isError) {
    return (
      <div className={cn("p-3", className)}>
        <ErrorState
          error={error}
          title={t("loadError")}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }
  if (rows.length === 0) {
    return (
      <div className={cn("p-3", className)}>
        <EmptyState
          icon={FileX2}
          title={t("empty.title")}
          body={t("empty.body")}
        />
      </div>
    );
  }

  return (
    <ScrollArea className={cn("h-full", className)}>
      <div
        ref={listRef}
        role="tree"
        aria-label={t("title")}
        tabIndex={-1}
        onKeyDown={onKeyDown}
        className="flex flex-col gap-px p-1 outline-none"
      >
        {rows.map(({ node, depth }) => {
          // `selected` in the molecule drives BOTH the highlight AND tabIndex=0
          // (roving tabindex), so exactly one row — the active focus row — is
          // tabbable. The active row defaults to the open file, so the open file
          // reads as highlighted; the dirty dot separately marks unsaved files.
          const isActiveTab = node.path === activePath;
          const isOpenFile = !node.isDir && node.path === selectedPath;
          return (
            <div key={node.path} data-path={node.path}>
              <FileTreeItem
                name={node.name}
                isDir={node.isDir}
                depth={depth}
                expanded={node.isDir ? expanded.has(node.path) : undefined}
                selected={isActiveTab}
                dirty={!node.isDir && dirtyPaths?.has(node.path)}
                className={cn(
                  // Keep the open file legibly marked even when focus moves
                  // elsewhere (a left accent bar), without a second highlight.
                  isOpenFile &&
                    !isActiveTab &&
                    "border-l-2 border-primary bg-accent/40"
                )}
                onSelect={() => {
                  setFocusedPath(node.path);
                  onSelect?.(node.path);
                }}
                onToggle={() => {
                  setFocusedPath(node.path);
                  toggle(node.path);
                }}
              />
            </div>
          );
        })}
      </div>
    </ScrollArea>
  );
}
