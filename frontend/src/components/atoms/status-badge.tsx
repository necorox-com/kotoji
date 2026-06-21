"use client";

/**
 * StatusBadge (atom) — design.md §3.1. The publish/draft/preview/etc. status
 * chip. CRITICAL a11y rule (§4.8): never color alone — always icon + text. The
 * status→color+icon map is the single source of truth so every surface reads the
 * same. Labels come from i18n (status.*).
 */

import {
  CircleDashed,
  CircleCheck,
  Eye,
  Hammer,
  TriangleAlert,
  WifiOff,
  type LucideIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

export type SiteStatus =
  | "published"
  | "draft"
  | "preview"
  | "building"
  | "error"
  | "stale"
  | "offline";

// Color mapping per design.md §3.1: published→success, draft→neutral,
// preview→info, building→info(+pulse), error→destructive, stale→warning.
const statusBadgeVariants = cva(
  "inline-flex h-5 w-fit shrink-0 items-center gap-1 rounded-sm border px-1.5 py-0.5 text-xs font-medium whitespace-nowrap [&>svg]:size-3 [&>svg]:shrink-0",
  {
    variants: {
      status: {
        published: "border-transparent bg-success/15 text-success",
        draft: "border-border bg-muted text-muted-foreground",
        preview: "border-transparent bg-info/15 text-info",
        building: "border-transparent bg-info/15 text-info",
        error: "border-transparent bg-destructive/15 text-destructive",
        stale: "border-transparent bg-warning/20 text-warning-foreground dark:text-warning",
        offline: "border-border bg-muted text-muted-foreground",
      },
    },
    defaultVariants: { status: "draft" },
  }
);

const STATUS_ICON: Record<SiteStatus, LucideIcon> = {
  published: CircleCheck,
  draft: CircleDashed,
  preview: Eye,
  building: Hammer,
  error: TriangleAlert,
  stale: TriangleAlert,
  offline: WifiOff,
};

export interface StatusBadgeProps
  extends Omit<React.ComponentProps<"span">, "children">,
    VariantProps<typeof statusBadgeVariants> {
  status: SiteStatus;
  /** Override the default i18n label (rarely needed). */
  label?: string;
}

export function StatusBadge({
  status,
  label,
  className,
  ...props
}: StatusBadgeProps) {
  const t = useTranslations("status");
  const Icon = STATUS_ICON[status];
  // "building" gets a subtle pulse to read as in-progress (design.md §3.1).
  const pulse = status === "building";
  return (
    <span
      data-slot="status-badge"
      className={cn(statusBadgeVariants({ status }), className)}
      {...props}
    >
      <Icon aria-hidden="true" className={cn(pulse && "animate-pulse")} />
      <span>{label ?? t(status)}</span>
    </span>
  );
}

export { statusBadgeVariants };
