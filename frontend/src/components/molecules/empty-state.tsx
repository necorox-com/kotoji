/**
 * EmptyState (molecule) — design.md §3.2. The empty half of the triplet: an
 * icon (24), heading, body, and a primary action. Friendly copy; a koto-bridge
 * line glyph illustration slot (the "人"-shaped mark) reinforces brand calm.
 */

import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

export interface EmptyStateProps {
  /** Lucide icon (rendered at 24). Falls back to the brand glyph if omitted. */
  icon?: LucideIcon;
  title: React.ReactNode;
  body?: React.ReactNode;
  /** Primary action node (usually a Button). */
  action?: React.ReactNode;
  className?: string;
}

/** The koto-bridge "人"-shaped brand glyph used as the default illustration. */
function BridgeGlyph() {
  return (
    <svg
      width="40"
      height="40"
      viewBox="0 0 56 56"
      fill="none"
      aria-hidden="true"
      className="text-muted-foreground/60"
    >
      <path
        d="M28 10 L44 44 M28 10 L12 44"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
      />
      <path
        d="M18 34 L38 34"
        stroke="var(--brand-gold)"
        strokeWidth="2.5"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function EmptyState({
  icon: Icon,
  title,
  body,
  action,
  className,
}: EmptyStateProps) {
  return (
    <div
      data-slot="empty-state"
      className={cn(
        "flex flex-col items-center justify-center gap-4 rounded-xl border border-dashed border-border px-6 py-16 text-center",
        className
      )}
    >
      <div className="flex size-12 items-center justify-center rounded-full bg-muted">
        {Icon ? (
          <Icon className="size-6 text-muted-foreground" aria-hidden="true" />
        ) : (
          <BridgeGlyph />
        )}
      </div>
      <div className="max-w-sm space-y-1">
        <p className="text-lg font-semibold text-foreground">{title}</p>
        {body ? <p className="text-sm text-muted-foreground">{body}</p> : null}
      </div>
      {action ? <div className="pt-1">{action}</div> : null}
    </div>
  );
}
