/**
 * Kbd (atom) — design.md §3.1. Keyboard shortcut hints (⌘K, ⌘S, Esc). Mono,
 * rounded-sm, hairline border. `data-slot="kbd"` lets the tooltip styles pick it
 * up (see ui/tooltip.tsx kbd selectors).
 */

import { cn } from "@/lib/utils";

export function Kbd({ className, ...props }: React.ComponentProps<"kbd">) {
  return (
    <kbd
      data-slot="kbd"
      className={cn(
        "inline-flex h-5 min-w-5 items-center justify-center gap-0.5 rounded-sm border border-border bg-muted px-1 font-mono text-[11px] font-medium text-muted-foreground",
        className
      )}
      {...props}
    />
  );
}
