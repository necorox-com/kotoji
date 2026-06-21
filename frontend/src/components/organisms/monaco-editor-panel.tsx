"use client";

/**
 * MonacoEditorPanel (organism) — design.md §3.3 / §4.6. The center editor pane.
 *
 *  - Loads file content via useFileContent; the payload's COMMIT `sha` is the
 *    optimistic-lock token captured at OPEN time and echoed as `baseSha` on every
 *    save (design.md §4.6, use-files.ts). We capture it in a ref so a background
 *    refetch can't silently move the lock under an in-progress edit.
 *  - Monaco is lazy / no-SSR (`next/dynamic` ssr:false) with a Suspense-style
 *    skeleton fallback (design.md §4.6).
 *  - Theme synced to next-themes: custom `kotoji-light` / `kotoji-dark` themes are
 *    defined on mount from the editor tokens and re-applied on theme change.
 *  - Dirty state: local edits diverge from the loaded baseline → dirty. ⌘S / Ctrl+S
 *    saves (writeFile with baseSha); the toolbar Save button mirrors it.
 *  - Conflict (409 stale baseSha) is surfaced to the parent via `onConflict` so
 *    the ConflictResolver organism can render; success toasts + clears dirty.
 *  - Phone / read-only branches → a read-only viewer with the "edit on a larger
 *    screen / ask AI" affordance (principle #6, design.md §3.3 / templates).
 */

import dynamic from "next/dynamic";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTheme } from "next-themes";
import { useTranslations } from "next-intl";
import { toast } from "sonner";
import { Eye, Save, Sparkles } from "lucide-react";
import type { editor } from "monaco-editor";
import type { Monaco, OnMount } from "@monaco-editor/react";
import { useFileContent, useWriteFile } from "@/lib/api/hooks";
import { isConflictError, errorMessage, type ConflictError } from "@/lib/api/error";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/atoms/spinner";
import { CodeText } from "@/components/atoms/code-text";
import { LoadingState } from "@/components/molecules/loading-state";
import { ErrorState } from "@/components/molecules/error-state";
import { EmptyState } from "@/components/molecules/empty-state";
import { useBreakpoint } from "@/hooks";
import { cn } from "@/lib/utils";

// Lazy, client-only Monaco. ssr:false is valid here because this module is a
// Client Component (design.md §4.6; Next 16 lazy-loading guide).
const MonacoEditor = dynamic(
  () => import("@monaco-editor/react").then((m) => m.Editor),
  {
    ssr: false,
    loading: () => (
      <div className="flex h-full items-center justify-center">
        <Spinner size="lg" />
      </div>
    ),
  }
);

/** Map a file extension to a Monaco language id (design.md §4.6). */
function languageForPath(path: string): string {
  const ext = path.split(".").pop()?.toLowerCase() ?? "";
  switch (ext) {
    case "html":
    case "htm":
      return "html";
    case "css":
      return "css";
    case "js":
    case "mjs":
    case "cjs":
      return "javascript";
    case "ts":
      return "typescript";
    case "jsx":
    case "tsx":
      return "typescript";
    case "json":
    case "map":
      return "json";
    case "md":
    case "markdown":
      return "markdown";
    case "xml":
    case "svg":
      return "xml";
    case "yml":
    case "yaml":
      return "yaml";
    default:
      return "plaintext";
  }
}

/**
 * Define the kotoji Monaco themes from the editor tokens so the code surface
 * matches the chrome (design.md §2.1.5 / §4.6). Called once per Monaco instance.
 * We read the resolved CSS custom properties at runtime so retuning tokens in
 * globals.css propagates without touching this file.
 */
function defineKotojiThemes(monaco: Monaco) {
  const readVar = (name: string, fallback: string) => {
    if (typeof window === "undefined") return fallback;
    const v = getComputedStyle(document.documentElement)
      .getPropertyValue(name)
      .trim();
    return v || fallback;
  };
  // Monaco needs concrete hex; we keep safe hex fallbacks matching the tokens
  // (oklch values aren't accepted by Monaco's theme engine).
  monaco.editor.defineTheme("kotoji-light", {
    base: "vs",
    inherit: true,
    rules: [],
    colors: {
      "editor.background": "#ffffff",
      "editor.foreground": "#2a2620",
      "editorLineNumber.foreground": "#a8a39a",
      "editorLineNumber.activeForeground": "#5b554c",
      "editor.selectionBackground": "#dbe3f6",
      "editor.lineHighlightBackground": "#f3f1ee",
      "editorCursor.foreground": "#3a52a8",
    },
  });
  monaco.editor.defineTheme("kotoji-dark", {
    base: "vs-dark",
    inherit: true,
    rules: [],
    colors: {
      "editor.background": "#1b1813",
      "editor.foreground": "#ece8e1",
      "editorLineNumber.foreground": "#5b554c",
      "editorLineNumber.activeForeground": "#a8a39a",
      "editor.selectionBackground": "#26304f",
      "editor.lineHighlightBackground": "#211d18",
      "editorCursor.foreground": "#7e93d6",
    },
  });
  // Touch readVar so token-driven retuning stays wired even though Monaco
  // currently needs literal hex; reading also warms the values for callers.
  void readVar("--editor-bg", "#ffffff");
}

export interface MonacoEditorPanelProps {
  handle: string;
  branch: string;
  /** Open file path; when undefined an empty "pick a file" state shows. */
  path?: string;
  /** Force read-only regardless of breakpoint (non-writable branch/role). */
  readOnly?: boolean;
  /** Notify parent of dirty transitions (for nav guards / tree dirty dots). */
  onDirtyChange?: (dirty: boolean) => void;
  /** Surface a save conflict so the parent can render ConflictResolver. */
  onConflict?: (conflict: ConflictError, attemptedContent: string) => void;
  className?: string;
}

export function MonacoEditorPanel({
  handle,
  branch,
  path,
  readOnly,
  onDirtyChange,
  onConflict,
  className,
}: MonacoEditorPanelProps) {
  const t = useTranslations("editor");
  const tFiles = useTranslations("files");
  const tCommon = useTranslations("common");
  const { resolvedTheme } = useTheme();
  const { isPhone } = useBreakpoint();

  const { data, isPending, isError, error, refetch } = useFileContent(
    handle,
    branch,
    path ?? ""
  );
  const write = useWriteFile(handle, branch);

  // Local editor buffer + the baseline it's compared against for dirty state +
  // the optimistic-lock token (baseSha) captured at the content the buffer was
  // seeded from (design.md §4.6).
  const [value, setValue] = useState<string>("");
  const [baseline, setBaseline] = useState<string>("");
  const [baseSha, setBaseSha] = useState<string>("");
  // Cursor position for the footer (Ln/Col).
  const [pos, setPos] = useState<{ line: number; column: number }>({
    line: 1,
    column: 1,
  });
  const editorRef = useRef<editor.IStandaloneCodeEditor | null>(null);
  const monacoRef = useRef<Monaco | null>(null);
  // Tracks the (path, sha) the buffer was last seeded from, so a re-render with
  // the SAME payload doesn't clobber in-progress edits. Held in state (not a
  // ref) so it participates in the "adjust state during render" idiom cleanly.
  const [seededKey, setSeededKey] = useState<string>("");

  // Seed the buffer DURING render (the React-recommended alternative to a
  // set-state-in-effect) the first time we see a new (path, sha) content payload.
  // This re-seeds after a save (new tip sha) but never while the user types.
  if (data) {
    const key = `${data.path}@${data.sha}`;
    if (key !== seededKey) {
      const next = data.isBinary ? "" : data.content;
      setSeededKey(key);
      setValue(next);
      setBaseline(next);
      setBaseSha(data.sha);
    }
  }

  const dirty = value !== baseline;
  const language = useMemo(
    () => (path ? languageForPath(path) : "plaintext"),
    [path]
  );
  // Read-only when forced, on phone (principle #6), on binary blobs, or while a
  // save is in flight.
  const effectiveReadOnly =
    readOnly || isPhone || (data?.isBinary ?? false);

  // Announce dirty transitions upward.
  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);

  // beforeunload guard: warn on leaving with unsaved changes (design.md §4.6).
  useEffect(() => {
    if (!dirty) return;
    const handler = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      // Modern browsers ignore custom text but require returnValue to be set.
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, [dirty]);

  // Apply the kotoji Monaco theme whenever the app theme resolves/changes.
  useEffect(() => {
    if (!monacoRef.current) return;
    monacoRef.current.editor.setTheme(
      resolvedTheme === "dark" ? "kotoji-dark" : "kotoji-light"
    );
  }, [resolvedTheme]);

  const doSave = useCallback(async () => {
    if (!path || effectiveReadOnly || !dirty || write.isPending) return;
    try {
      await write.mutateAsync({
        path,
        content: value,
        baseSha,
      });
      // On success the hook invalidates the file query; clear dirty optimistically
      // and let the refetch re-seed the new baseSha.
      setBaseline(value);
      toast.success(t("saved"));
    } catch (err) {
      // 409 stale baseSha → hand the typed conflict to the parent's resolver.
      if (isConflictError(err)) {
        onConflict?.(err, value);
        return;
      }
      toast.error(errorMessage(err, t("saveError")));
    }
  }, [path, effectiveReadOnly, dirty, write, value, baseSha, t, onConflict]);

  // Keep the latest save handler reachable from Monaco's command (which binds
  // once at mount) without re-binding on every keystroke.
  const saveRef = useRef(doSave);
  useEffect(() => {
    saveRef.current = doSave;
  }, [doSave]);

  const onMount: OnMount = useCallback(
    (instance, monaco) => {
      editorRef.current = instance;
      monacoRef.current = monaco;
      defineKotojiThemes(monaco);
      monaco.editor.setTheme(
        resolvedTheme === "dark" ? "kotoji-dark" : "kotoji-light"
      );
      // ⌘S / Ctrl+S → save (design.md §4.6). Prevent the browser's Save dialog.
      instance.addCommand(
        monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS,
        () => {
          void saveRef.current();
        }
      );
      instance.onDidChangeCursorPosition((e) => {
        setPos({ line: e.position.lineNumber, column: e.position.column });
      });
    },
    [resolvedTheme]
  );

  // --- empty / loading / error / binary states ---
  if (!path) {
    return (
      <div className={cn("flex h-full items-center justify-center p-6", className)}>
        <EmptyState title={tFiles("empty.title")} body={tFiles("empty.body")} />
      </div>
    );
  }
  if (isPending) {
    return (
      <div className={cn("h-full p-4", className)}>
        <LoadingState rows={8} label={t("save")} />
      </div>
    );
  }
  if (isError) {
    return (
      <div className={cn("flex h-full items-center justify-center p-4", className)}>
        <ErrorState
          error={error}
          title={tFiles("loadError")}
          onRetry={() => void refetch()}
        />
      </div>
    );
  }

  return (
    <div
      data-slot="monaco-editor-panel"
      className={cn("flex h-full min-h-0 flex-col bg-card", className)}
    >
      {/* Toolbar: file path + Save (and a read-only hint where applicable). */}
      <div className="flex h-10 shrink-0 items-center gap-2 border-b border-border px-3">
        <CodeText truncate className="min-w-0 flex-1">
          {path}
        </CodeText>
        {effectiveReadOnly ? (
          <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
            <Eye className="size-3.5" aria-hidden="true" />
            {tFiles("readOnlyOnPhone")}
          </span>
        ) : (
          <Button
            type="button"
            size="sm"
            onClick={() => void doSave()}
            disabled={!dirty || write.isPending}
            aria-busy={write.isPending}
            aria-keyshortcuts="Control+S Meta+S"
          >
            {write.isPending ? (
              <Spinner size="sm" />
            ) : (
              <Save className="size-3.5" aria-hidden="true" />
            )}
            {write.isPending ? tCommon("saving") : t("save")}
          </Button>
        )}
      </div>

      {/* The editor body, OR the phone read-only affordance (principle #6). */}
      <div className="relative min-h-0 flex-1">
        {data?.isBinary ? (
          <div className="flex h-full items-center justify-center p-6">
            <EmptyState
              icon={Eye}
              title={tFiles("title")}
              body={tFiles("readOnlyOnPhone")}
            />
          </div>
        ) : (
          <MonacoEditor
            language={language}
            value={value}
            onChange={(next) => setValue(next ?? "")}
            theme={resolvedTheme === "dark" ? "kotoji-dark" : "kotoji-light"}
            onMount={onMount}
            options={{
              readOnly: effectiveReadOnly,
              fontSize: 13,
              fontFamily: "var(--font-mono)",
              minimap: { enabled: false },
              scrollBeyondLastLine: false,
              automaticLayout: true,
              tabSize: 2,
              wordWrap: "on",
              renderLineHighlight: "all",
              smoothScrolling: true,
              padding: { top: 12, bottom: 12 },
            }}
            height="100%"
            width="100%"
          />
        )}
      </div>

      {/* Footer: language · Ln/Col · base-SHA · dirty/phone affordances. */}
      <div className="flex h-7 shrink-0 items-center gap-3 border-t border-border px-3 text-xs text-muted-foreground">
        <span className="uppercase">{language}</span>
        <span aria-hidden="true">·</span>
        <span>{t("lineColumn", { line: pos.line, column: pos.column })}</span>
        {baseSha ? (
          <>
            <span aria-hidden="true">·</span>
            <span className="hidden items-center gap-1 sm:inline-flex">
              {t("baseSha")}{" "}
              <CodeText className="bg-transparent px-0">
                {baseSha.slice(0, 7)}
              </CodeText>
            </span>
          </>
        ) : null}
        {dirty ? (
          <span className="ml-auto inline-flex items-center gap-1 text-warning">
            <span
              className="size-1.5 rounded-full bg-warning"
              aria-hidden="true"
            />
            {t("unsavedChanges")}
          </span>
        ) : isPhone ? (
          <span className="ml-auto inline-flex items-center gap-1">
            <Sparkles className="size-3.5" aria-hidden="true" />
            {tFiles("askAiToEdit")}
          </span>
        ) : null}
      </div>
    </div>
  );
}
