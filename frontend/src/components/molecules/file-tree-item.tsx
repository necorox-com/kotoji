"use client";

/**
 * FileTreeItem (molecule) — design.md §3.2. One row of the FileTree: chevron (for
 * dirs) + FileTypeIcon + name + optional dirty-dot (unsaved). Indents by depth;
 * selected/hover states; keyboard-focusable (the FileTree organism owns arrow
 * navigation, §3.3). Pure presentational — emits onSelect/onToggle.
 */

import { ChevronRight } from "lucide-react";
import { useTranslations } from "next-intl";
import { FileTypeIcon } from "./file-type-icon";
import { cn } from "@/lib/utils";

export interface FileTreeItemProps {
  name: string;
  isDir?: boolean;
  depth?: number;
  selected?: boolean;
  /** For dirs: expanded state (drives chevron + open-folder icon). */
  expanded?: boolean;
  /** Unsaved-changes indicator (right-aligned dot). */
  dirty?: boolean;
  onSelect?: () => void;
  /** For dirs: toggle expand/collapse. */
  onToggle?: () => void;
  className?: string;
}

export function FileTreeItem({
  name,
  isDir,
  depth = 0,
  selected,
  expanded,
  dirty,
  onSelect,
  onToggle,
  className,
}: FileTreeItemProps) {
  const t = useTranslations("a11y");

  return (
    <div
      role="treeitem"
      aria-selected={selected}
      aria-expanded={isDir ? !!expanded : undefined}
      tabIndex={selected ? 0 : -1}
      onClick={() => (isDir ? onToggle?.() : onSelect?.())}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          if (isDir) onToggle?.();
          else onSelect?.();
        }
      }}
      // Indent by depth (12px per level). Inline style is the simplest correct
      // way to express arbitrary depth without generating N utility classes.
      style={{ paddingInlineStart: `${depth * 12 + 8}px` }}
      className={cn(
        "flex h-8 cursor-pointer items-center gap-1.5 rounded-md pr-2 text-sm transition-colors",
        "focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
        selected
          ? "bg-accent text-accent-foreground"
          : "text-foreground hover:bg-accent/50",
        className
      )}
    >
      {isDir ? (
        <ChevronRight
          className={cn(
            "size-3.5 shrink-0 text-muted-foreground transition-transform",
            expanded && "rotate-90"
          )}
          aria-label={expanded ? t("collapse") : t("expand")}
        />
      ) : (
        // Spacer to align files with directory labels.
        <span className="size-3.5 shrink-0" aria-hidden="true" />
      )}
      <FileTypeIcon name={name} isDir={isDir} open={expanded} />
      <span className="min-w-0 flex-1 truncate">{name}</span>
      {dirty ? (
        <span
          className="size-1.5 shrink-0 rounded-full bg-warning"
          aria-label={t("statusDot")}
        />
      ) : null}
    </div>
  );
}
