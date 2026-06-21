"use client";

/**
 * Spinner (atom) — design.md §3.1. Lucide Loader2 + animate-spin, with a
 * `role="status"` and sr-only label so async state is announced (§4.8). Reduced
 * motion is honored globally by the CSS guard in globals.css (§4.7), which
 * collapses the spin; we keep the element visible as a static indicator.
 */

import { Loader2 } from "lucide-react";
import { useTranslations } from "next-intl";
import { cn } from "@/lib/utils";

const SIZE_CLASS = {
  sm: "size-4",
  md: "size-5",
  lg: "size-6",
} as const;

export interface SpinnerProps {
  size?: keyof typeof SIZE_CLASS;
  /** Visible-to-AT label; defaults to the i18n "working" string. */
  label?: string;
  className?: string;
}

export function Spinner({ size = "md", label, className }: SpinnerProps) {
  const t = useTranslations("a11y");
  const text = label ?? t("spinner");
  return (
    <span role="status" className={cn("inline-flex items-center", className)}>
      <Loader2 className={cn("animate-spin text-muted-foreground", SIZE_CLASS[size])} aria-hidden="true" />
      <span className="sr-only">{text}</span>
    </span>
  );
}
