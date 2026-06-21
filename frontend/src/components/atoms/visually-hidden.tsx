/**
 * VisuallyHidden (atom) — design.md §3.1. Content available to screen readers
 * but visually hidden (sr-only). Use for accessible names that would be visual
 * noise (e.g. a heading required by a dialog but shown via an icon).
 */

import { cn } from "@/lib/utils";

export function VisuallyHidden({
  className,
  ...props
}: React.ComponentProps<"span">) {
  return <span className={cn("sr-only", className)} {...props} />;
}
