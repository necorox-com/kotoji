"use client";

/**
 * UploadDropzone (organism) — design.md §3.3. Drag-and-drop a .zip → upload via
 * useUploadZip (XHR, real progress) → import as one commit. Used in CreateSite
 * and the project detail "re-upload" flow.
 *
 * Client-side pre-checks (UX sugar; the SERVER is the security authority for
 * ZipSlip / zip-bomb / extension allow-list — design.md §3.3, §5 gap #8):
 *   - extension must be `.zip`,
 *   - soft size warning against `config.maxUploadBytes` (fetched, NOT hardcoded).
 * Server rejections surface clearly via an inline Alert (the typed ApiError
 * message). Progress bar + percent while uploading; success → toast + onUploaded.
 *
 * a11y: the dropzone is a real <button> (keyboard-openable file picker), drag
 * state is announced visually + via aria, and the hidden <input type=file> is the
 * canonical trigger (design.md §4.8).
 */

import { useId, useRef, useState } from "react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";
import { TriangleAlert, UploadCloud } from "lucide-react";
import { useConfig } from "@/lib/api/hooks";
import { useUploadZip } from "@/lib/api/hooks";
import { errorMessage } from "@/lib/api/error";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Spinner } from "@/components/atoms/spinner";
import { cn } from "@/lib/utils";

/** Format a byte count into a short human string for limit messaging. */
function formatBytes(bytes: number): string {
  if (bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const i = Math.min(
    units.length - 1,
    Math.floor(Math.log(bytes) / Math.log(1024))
  );
  const value = bytes / Math.pow(1024, i);
  return `${value.toFixed(value >= 10 || i === 0 ? 0 : 1)} ${units[i]}`;
}

export interface UploadDropzoneProps {
  handle: string;
  branch: string;
  /**
   * REQUIRED current branch tip (optimistic lock), EXCEPT when seeding an empty
   * branch for the first time — then omit it (use-upload.ts UploadZipArgs).
   */
  baseSha?: string;
  /** Commit message for the import. */
  message?: string;
  /** Called with the resulting commit SHA on success. */
  onUploaded?: (commitSha: string) => void;
  className?: string;
}

export function UploadDropzone({
  handle,
  branch,
  baseSha,
  message,
  onUploaded,
  className,
}: UploadDropzoneProps) {
  const t = useTranslations("upload");
  const inputId = useId();
  const inputRef = useRef<HTMLInputElement>(null);
  const [dragging, setDragging] = useState(false);
  const [progress, setProgress] = useState(0); // 0..100
  const [clientError, setClientError] = useState<string | null>(null);

  const { data: config } = useConfig();
  const upload = useUploadZip(handle, branch);
  const maxBytes = config?.maxUploadBytes;

  /** Validate then upload a chosen/dropped file. */
  const handleFile = async (file: File) => {
    setClientError(null);
    // Pre-check 1: extension must be .zip (case-insensitive).
    if (!file.name.toLowerCase().endsWith(".zip")) {
      setClientError(t("invalidType"));
      return;
    }
    // Pre-check 2: soft size guard against the fetched server limit.
    if (maxBytes !== undefined && file.size > maxBytes) {
      setClientError(`${t("tooLarge")}（${formatBytes(maxBytes)}）`);
      return;
    }

    setProgress(0);
    try {
      const commit = await upload.mutateAsync({
        file,
        ...(baseSha ? { baseSha } : {}),
        ...(message ? { message } : {}),
        onProgress: (fraction) => setProgress(Math.round(fraction * 100)),
      });
      toast.success(t("success"));
      onUploaded?.(commit.sha);
    } catch (err) {
      // Server rejection (ZipSlip / bomb / extension / size) → inline message.
      setClientError(errorMessage(err, t("error")));
    } finally {
      setProgress(0);
      // Reset the input so re-selecting the same file fires onChange again.
      if (inputRef.current) inputRef.current.value = "";
    }
  };

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault();
    setDragging(false);
    if (upload.isPending) return;
    const file = e.dataTransfer.files?.[0];
    if (file) void handleFile(file);
  };

  const openPicker = () => {
    if (upload.isPending) return;
    inputRef.current?.click();
  };

  return (
    <div data-slot="upload-dropzone" className={cn("space-y-3", className)}>
      {/* The drop target is a button so it's keyboard-operable (Enter/Space). */}
      <button
        type="button"
        onClick={openPicker}
        onDragOver={(e) => {
          e.preventDefault();
          if (!upload.isPending) setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={onDrop}
        aria-describedby={`${inputId}-help`}
        aria-disabled={upload.isPending}
        className={cn(
          "flex w-full flex-col items-center justify-center gap-3 rounded-xl border-2 border-dashed px-6 py-10 text-center transition-colors",
          "focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-none",
          dragging
            ? "border-primary bg-accent"
            : "border-input bg-muted/40 hover:bg-muted",
          upload.isPending && "pointer-events-none opacity-70"
        )}
      >
        {upload.isPending ? (
          <Spinner size="lg" />
        ) : (
          <UploadCloud
            className="size-6 text-muted-foreground"
            aria-hidden="true"
          />
        )}
        <span className="text-sm font-medium text-foreground">
          {upload.isPending ? t("uploading") : t("dropzone")}
        </span>
        {!upload.isPending ? (
          <span className="flex items-center gap-2 text-xs text-muted-foreground">
            <span>{t("or")}</span>
            {/* Render the explicit "choose file" as a styled span (the whole
                surface is already the button) to keep one focusable target. */}
            <span className="font-medium text-primary underline-offset-4 group-hover:underline">
              {t("chooseFile")}
            </span>
          </span>
        ) : null}
      </button>

      {/* The canonical hidden file input (UploadDropzoneTrigger, design §3.2). */}
      <input
        ref={inputRef}
        id={inputId}
        type="file"
        accept=".zip,application/zip,application/x-zip-compressed"
        className="sr-only"
        onChange={(e) => {
          const file = e.target.files?.[0];
          if (file) void handleFile(file);
        }}
      />

      {/* Limit hint (accurate, fetched from config). */}
      <p id={`${inputId}-help`} className="text-xs text-muted-foreground">
        {maxBytes !== undefined
          ? `.zip · ${formatBytes(maxBytes)}`
          : ".zip"}
      </p>

      {/* Progress while uploading (announced via role=progressbar). */}
      {upload.isPending ? (
        <div
          role="progressbar"
          aria-valuenow={progress}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label={t("uploading")}
          className="space-y-1"
        >
          <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
            <div
              className="h-full rounded-full bg-primary transition-all"
              style={{ width: `${progress}%` }}
            />
          </div>
          <p className="text-right text-xs text-muted-foreground tabular-nums">
            {t("progress", { percent: progress })}
          </p>
        </div>
      ) : null}

      {/* Client/server rejection surfaced inline (design §3.3). */}
      {clientError ? (
        <Alert variant="destructive">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription>{clientError}</AlertDescription>
        </Alert>
      ) : null}
    </div>
  );
}
