"use client";

/**
 * DiffViewer (organism) — design.md §3.3. A read-only Monaco DiffEditor with a
 * header (from → to) + DiffStat. Side-by-side on desktop, unified on phone
 * (`renderSideBySide={isDesktop}`, design.md §3.3). Two modes:
 *
 *  1. CONTENT mode — caller passes explicit `original`/`modified` strings (used
 *     by ConflictResolver: server version ↔ your version).
 *  2. REFS mode — caller passes `from`/`to` refs and a `path`; the organism
 *     fetches the diff via useDiff and renders the chosen file's unified patch
 *     reconstructed into before/after. Used by History (commit diff) and the
 *     Publish preview (draft ↔ published).
 *
 * Lazy / no-SSR Monaco (design.md §4.6); theme synced to next-themes.
 */

import dynamic from "next/dynamic";
import { useEffect, useMemo, useRef } from "react";
import { useTheme } from "next-themes";
import { useTranslations } from "next-intl";
import { ArrowRight } from "lucide-react";
import type { Monaco, DiffOnMount } from "@monaco-editor/react";
import { useDiff } from "@/lib/api/hooks";
import type { FileDiff } from "@/lib/api/types";
import { Spinner } from "@/components/atoms/spinner";
import { LoadingState } from "@/components/molecules/loading-state";
import { ErrorState } from "@/components/molecules/error-state";
import { EmptyState } from "@/components/molecules/empty-state";
import { useBreakpoint } from "@/hooks";
import { cn } from "@/lib/utils";

const MonacoDiffEditor = dynamic(
  () => import("@monaco-editor/react").then((m) => m.DiffEditor),
  {
    ssr: false,
    loading: () => (
      <div className="flex h-full items-center justify-center">
        <Spinner size="lg" />
      </div>
    ),
  }
);

/** Same language map used by the editor (kept local to avoid cross-organism deps). */
function languageForPath(path: string): string {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  if (["html", "htm"].includes(ext)) return "html";
  if (ext === "css") return "css";
  if (["js", "mjs", "cjs"].includes(ext)) return "javascript";
  if (["ts", "tsx", "jsx"].includes(ext)) return "typescript";
  if (["json", "map"].includes(ext)) return "json";
  if (["md", "markdown"].includes(ext)) return "markdown";
  if (["xml", "svg"].includes(ext)) return "xml";
  if (["yml", "yaml"].includes(ext)) return "yaml";
  return "plaintext";
}

/**
 * Reconstruct before/after text from a unified patch so Monaco's DiffEditor can
 * render it. We keep `+`/`-`/context lines and strip the leading marker. This is
 * a pragmatic reconstruction (the API ships a `unifiedPatch`, not raw blobs); it
 * is exact for line-level diffs which is what the patch encodes.
 */
function splitUnifiedPatch(patch: string): { before: string; after: string } {
  const before: string[] = [];
  const after: string[] = [];
  for (const line of patch.split("\n")) {
    // Skip hunk headers and file headers; they aren't content.
    if (
      line.startsWith("@@") ||
      line.startsWith("+++") ||
      line.startsWith("---") ||
      line.startsWith("diff ") ||
      line.startsWith("index ")
    ) {
      continue;
    }
    const marker = line[0];
    const rest = line.slice(1);
    if (marker === "+") {
      after.push(rest);
    } else if (marker === "-") {
      before.push(rest);
    } else {
      // Context line (leading space) or empty — present on both sides.
      before.push(rest);
      after.push(rest);
    }
  }
  return { before: before.join("\n"), after: after.join("\n") };
}

function defineKotojiDiffThemes(monaco: Monaco) {
  monaco.editor.defineTheme("kotoji-light", {
    base: "vs",
    inherit: true,
    rules: [],
    colors: {
      "editor.background": "#ffffff",
      "editor.foreground": "#2a2620",
      "diffEditor.insertedTextBackground": "#2e9e5b22",
      "diffEditor.removedTextBackground": "#c0392b22",
    },
  });
  monaco.editor.defineTheme("kotoji-dark", {
    base: "vs-dark",
    inherit: true,
    rules: [],
    colors: {
      "editor.background": "#1b1813",
      "editor.foreground": "#ece8e1",
      "diffEditor.insertedTextBackground": "#5cd98a22",
      "diffEditor.removedTextBackground": "#ef827822",
    },
  });
}

/** DiffStat — the +adds / −dels pair (mirrors the molecule of the same intent). */
function DiffStat({
  additions,
  deletions,
}: {
  additions: number;
  deletions: number;
}) {
  return (
    <span className="inline-flex shrink-0 items-center gap-1.5 font-mono text-xs">
      <span className="text-success">+{additions}</span>
      <span className="text-destructive">−{deletions}</span>
    </span>
  );
}

interface DiffViewerCommonProps {
  /** Header labels for the two sides (e.g. "あなたの版" / "サーバーの版"). */
  fromLabel?: string;
  toLabel?: string;
  /** Editor height; defaults to filling the parent. */
  height?: number | string;
  className?: string;
}

interface DiffViewerContentProps extends DiffViewerCommonProps {
  mode: "content";
  /** Left (original) text. */
  original: string;
  /** Right (modified) text. */
  modified: string;
  /** Path drives syntax highlighting language. */
  path?: string;
  additions?: number;
  deletions?: number;
}

interface DiffViewerRefsProps extends DiffViewerCommonProps {
  mode: "refs";
  handle: string;
  from: string;
  to?: string;
  /** Which file's diff to show (the DiffResult carries many). */
  path: string;
}

export type DiffViewerProps = DiffViewerContentProps | DiffViewerRefsProps;

/** The Monaco-rendering inner view (shared by both modes). */
function DiffView({
  original,
  modified,
  language,
  fromLabel,
  toLabel,
  additions,
  deletions,
  height,
  className,
}: {
  original: string;
  modified: string;
  language: string;
  fromLabel?: string;
  toLabel?: string;
  additions: number;
  deletions: number;
  height?: number | string;
  className?: string;
}) {
  const t = useTranslations("history");
  const { resolvedTheme } = useTheme();
  const { isDesktop } = useBreakpoint();
  const monacoRef = useRef<Monaco | null>(null);

  useEffect(() => {
    if (!monacoRef.current) return;
    monacoRef.current.editor.setTheme(
      resolvedTheme === "dark" ? "kotoji-dark" : "kotoji-light"
    );
  }, [resolvedTheme]);

  const onMount: DiffOnMount = (_editor, monaco) => {
    monacoRef.current = monaco;
    defineKotojiDiffThemes(monaco);
    monaco.editor.setTheme(
      resolvedTheme === "dark" ? "kotoji-dark" : "kotoji-light"
    );
  };

  return (
    <div
      data-slot="diff-viewer"
      className={cn(
        "flex min-h-0 flex-col overflow-hidden rounded-lg border border-border bg-card",
        className
      )}
    >
      <div className="flex h-9 shrink-0 items-center gap-2 border-b border-border px-3 text-xs">
        <span className="truncate text-muted-foreground">
          {fromLabel ?? t("viewDiff")}
        </span>
        <ArrowRight
          className="size-3.5 shrink-0 text-muted-foreground"
          aria-hidden="true"
        />
        <span className="truncate font-medium text-foreground">
          {toLabel ?? ""}
        </span>
        <span className="ml-auto">
          <DiffStat additions={additions} deletions={deletions} />
        </span>
      </div>
      <div className="min-h-0 flex-1" style={{ height }}>
        <MonacoDiffEditor
          original={original}
          modified={modified}
          language={language}
          theme={resolvedTheme === "dark" ? "kotoji-dark" : "kotoji-light"}
          onMount={onMount}
          options={{
            readOnly: true,
            renderSideBySide: isDesktop, // unified on phone/tablet (§3.3)
            fontSize: 13,
            fontFamily: "var(--font-mono)",
            minimap: { enabled: false },
            scrollBeyondLastLine: false,
            automaticLayout: true,
            wordWrap: "on",
            renderOverviewRuler: false,
          }}
          height="100%"
          width="100%"
        />
      </div>
    </div>
  );
}

export function DiffViewer(props: DiffViewerProps) {
  if (props.mode === "content") return <DiffViewerContent {...props} />;
  return <DiffViewerRefs {...props} />;
}

function DiffViewerContent({
  original,
  modified,
  path,
  fromLabel,
  toLabel,
  additions,
  deletions,
  height,
  className,
}: DiffViewerContentProps) {
  const language = useMemo(
    () => (path ? languageForPath(path) : "plaintext"),
    [path]
  );
  // Derive a stat when the caller didn't pass one (cheap line-count delta).
  const stat = useMemo(() => {
    if (additions !== undefined && deletions !== undefined) {
      return { additions, deletions };
    }
    const before = original ? original.split("\n").length : 0;
    const after = modified ? modified.split("\n").length : 0;
    return {
      additions: Math.max(0, after - before),
      deletions: Math.max(0, before - after),
    };
  }, [original, modified, additions, deletions]);

  return (
    <DiffView
      original={original}
      modified={modified}
      language={language}
      fromLabel={fromLabel}
      toLabel={toLabel}
      additions={stat.additions}
      deletions={stat.deletions}
      height={height}
      className={className}
    />
  );
}

function DiffViewerRefs({
  handle,
  from,
  to,
  path,
  fromLabel,
  toLabel,
  height,
  className,
}: DiffViewerRefsProps) {
  const t = useTranslations("history");
  const { data, isPending, isError, error, refetch } = useDiff(
    handle,
    from,
    to,
    { path }
  );

  const fileDiff: FileDiff | undefined = useMemo(
    () => data?.files.find((f) => f.path === path) ?? data?.files[0],
    [data, path]
  );
  const reconstructed = useMemo(() => {
    if (!fileDiff?.unifiedPatch) return { before: "", after: "" };
    return splitUnifiedPatch(fileDiff.unifiedPatch);
  }, [fileDiff]);

  const language = useMemo(() => languageForPath(path), [path]);

  if (isPending) {
    return (
      <div className={cn("p-4", className)}>
        <LoadingState rows={6} label={t("viewDiff")} />
      </div>
    );
  }
  if (isError) {
    return (
      <div className={cn("p-4", className)}>
        <ErrorState
          error={error}
          title={t("loadError")}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }
  if (!fileDiff) {
    return (
      <div className={cn("p-4", className)}>
        <EmptyState title={t("empty.title")} body={t("empty.body")} />
      </div>
    );
  }
  // Binary blobs have no text patch to render side-by-side.
  if (fileDiff.isBinary) {
    return (
      <div className={cn("p-4", className)}>
        <EmptyState title={fileDiff.path} body={t("viewDiff")} />
      </div>
    );
  }

  return (
    <DiffView
      original={reconstructed.before}
      modified={reconstructed.after}
      language={language}
      fromLabel={fromLabel ?? from}
      toLabel={toLabel ?? to ?? path}
      additions={fileDiff.additions}
      deletions={fileDiff.deletions}
      height={height}
      className={className}
    />
  );
}
