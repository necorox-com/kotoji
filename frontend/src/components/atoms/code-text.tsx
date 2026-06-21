/**
 * CodeText / InlineCode (atom) — design.md §3.1. Inline mono for handle / SHA /
 * path / URL. Selectable, subtle muted background, rounded-sm. Used inside
 * CopyableUrl, CommitItem (short SHA), FileBreadcrumb, etc.
 */

import { cn } from "@/lib/utils";

export interface CodeTextProps extends React.ComponentProps<"code"> {
  /** Truncate with ellipsis (callers usually pair with a Tooltip for the full value). */
  truncate?: boolean;
}

export function CodeText({ className, truncate, ...props }: CodeTextProps) {
  return (
    <code
      data-slot="code-text"
      className={cn(
        "rounded-sm bg-muted px-1 py-0.5 font-mono text-[13px] leading-snug text-foreground",
        truncate && "inline-block max-w-full truncate align-bottom",
        className
      )}
      {...props}
    />
  );
}

// Alias used by some call sites (design inventory lists both names).
export const InlineCode = CodeText;
