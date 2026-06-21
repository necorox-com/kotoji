/**
 * SectionHeading (atom) — design.md §3.1 (SectionHeading) / typography §2.3.
 * A titled section header with an optional description and right-aligned action
 * slot (e.g. a "新しいサイト" button on the Dashboard heading). Renders a real
 * heading element at the requested level for document outline a11y (§4.8).
 */

import { cn } from "@/lib/utils";

const LEVEL_CLASS = {
  1: "text-3xl font-bold tracking-tight", // text-h1
  2: "text-2xl font-semibold tracking-tight", // text-h2
  3: "text-xl font-semibold", // text-h3
  4: "text-lg font-semibold", // text-h4
} as const;

export interface SectionHeadingProps
  extends Omit<React.ComponentProps<"div">, "title"> {
  title: React.ReactNode;
  description?: React.ReactNode;
  /** Heading level for the document outline (default 2). */
  level?: 1 | 2 | 3 | 4;
  /** Right-aligned actions (buttons, menus). */
  actions?: React.ReactNode;
}

export function SectionHeading({
  title,
  description,
  level = 2,
  actions,
  className,
  ...props
}: SectionHeadingProps) {
  const Tag = `h${level}` as const;
  return (
    <div
      data-slot="section-heading"
      className={cn(
        "flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between",
        className
      )}
      {...props}
    >
      <div className="min-w-0 space-y-1">
        <Tag className={cn(LEVEL_CLASS[level], "text-foreground")}>{title}</Tag>
        {description ? (
          <p className="text-sm text-muted-foreground">{description}</p>
        ) : null}
      </div>
      {actions ? (
        <div className="flex shrink-0 items-center gap-2">{actions}</div>
      ) : null}
    </div>
  );
}
