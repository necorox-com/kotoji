"use client";

/**
 * Chip (atom) — design.md §3.1/§3.2. An outline Badge that can be removable
 * (filter chips, branch chips). When `onRemove` is provided it renders an
 * accessible ✕ button (aria-label from i18n a11y.removeChip).
 */

import { X } from "lucide-react";
import { useTranslations } from "next-intl";
import { cn } from "@/lib/utils";

export interface ChipProps extends React.ComponentProps<"span"> {
  /** Show + wire a remove (✕) affordance. */
  onRemove?: () => void;
  /** Visual emphasis: neutral outline (default) or selected/active. */
  active?: boolean;
}

export function Chip({
  className,
  children,
  onRemove,
  active,
  ...props
}: ChipProps) {
  const t = useTranslations("a11y");
  return (
    <span
      data-slot="chip"
      className={cn(
        "inline-flex h-6 items-center gap-1 rounded-sm border px-2 text-xs font-medium",
        active
          ? "border-transparent bg-accent text-accent-foreground"
          : "border-border bg-background text-foreground",
        className
      )}
      {...props}
    >
      <span className="truncate">{children}</span>
      {onRemove ? (
        <button
          type="button"
          onClick={onRemove}
          aria-label={t("removeChip")}
          className="-mr-0.5 inline-flex size-4 items-center justify-center rounded-sm text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <X className="size-3" aria-hidden="true" />
        </button>
      ) : null}
    </span>
  );
}
