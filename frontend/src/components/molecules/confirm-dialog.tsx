"use client";

/**
 * ConfirmDialog (molecule) — design.md §3.2. Reusable confirm for destructive /
 * irreversible-ish actions (delete site, rollback, publish). Controlled via
 * `open`/`onOpenChange`. Supports a `variant` (destructive vs default) and an
 * optional `confirmPhrase` typed-confirmation (delete requires typing the
 * handle). The confirm button shows a loading state while `loading`.
 */

import { useState } from "react";
import { useTranslations } from "next-intl";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/atoms";

export interface ConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  variant?: "default" | "destructive";
  /** Disable confirm until the user types this exact phrase (e.g. the handle). */
  confirmPhrase?: string;
  /** Prompt above the typed-confirmation input. */
  confirmPhraseLabel?: React.ReactNode;
  /** Async confirm handler; dialog stays open while it runs (loading). */
  onConfirm: () => void | Promise<void>;
  loading?: boolean;
}

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  cancelLabel,
  variant = "default",
  confirmPhrase,
  confirmPhraseLabel,
  onConfirm,
  loading = false,
}: ConfirmDialogProps) {
  const t = useTranslations("common");
  const [typed, setTyped] = useState("");

  const phraseOk = !confirmPhrase || typed === confirmPhrase;
  const confirmDisabled = loading || !phraseOk;

  // Reset the typed phrase on every open/close transition (no effect needed) so
  // a stale match can't leak across opens.
  const handleOpenChange = (next: boolean) => {
    setTyped("");
    onOpenChange(next);
  };

  return (
    <AlertDialog open={open} onOpenChange={handleOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          {description ? (
            <AlertDialogDescription>{description}</AlertDialogDescription>
          ) : null}
        </AlertDialogHeader>

        {confirmPhrase ? (
          <div className="space-y-1.5">
            {confirmPhraseLabel ? (
              <label
                htmlFor="confirm-phrase"
                className="text-sm text-muted-foreground"
              >
                {confirmPhraseLabel}
              </label>
            ) : null}
            <Input
              id="confirm-phrase"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoComplete="off"
              className="font-mono"
            />
          </div>
        ) : null}

        <AlertDialogFooter>
          <AlertDialogCancel disabled={loading}>
            {cancelLabel ?? t("cancel")}
          </AlertDialogCancel>
          <AlertDialogAction
            variant={variant === "destructive" ? "destructive" : "default"}
            disabled={confirmDisabled}
            aria-busy={loading}
            // Keep the dialog open while the async action runs; the caller closes
            // it on success via onOpenChange(false).
            onClick={(e) => {
              e.preventDefault();
              void onConfirm();
            }}
          >
            {loading ? <Spinner size="sm" /> : null}
            {confirmLabel ?? t("confirm")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
