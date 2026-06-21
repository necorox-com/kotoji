/**
 * Link (atom) — design.md §3.1. A Next <Link> wrapper with nav/inline variants.
 * inline = indigo, underline on hover; nav = quiet, used in sidebar/breadcrumbs.
 * External links get an icon + new-tab rel hardening (security).
 */

import NextLink from "next/link";
import { ArrowUpRight } from "lucide-react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

const linkVariants = cva(
  "rounded-sm transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
  {
    variants: {
      variant: {
        inline: "text-primary underline-offset-4 hover:underline",
        nav: "text-foreground hover:text-primary",
        muted: "text-muted-foreground hover:text-foreground",
      },
    },
    defaultVariants: { variant: "inline" },
  }
);

export interface LinkProps
  extends React.ComponentProps<typeof NextLink>,
    VariantProps<typeof linkVariants> {
  /** Render as an external link (target=_blank + rel hardening + icon). */
  external?: boolean;
}

export function Link({
  className,
  variant,
  external,
  children,
  ...props
}: LinkProps) {
  // External links open in a new tab and get noopener/noreferrer (prevents
  // reverse-tabnabbing — security best practice).
  const externalProps = external
    ? { target: "_blank", rel: "noopener noreferrer" }
    : {};
  return (
    <NextLink
      className={cn(linkVariants({ variant }), external && "inline-flex items-center gap-0.5", className)}
      {...externalProps}
      {...props}
    >
      {children}
      {external ? (
        <ArrowUpRight className="size-3.5 shrink-0" aria-hidden="true" />
      ) : null}
    </NextLink>
  );
}

export { linkVariants };
