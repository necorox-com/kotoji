/**
 * AuthLayout (template) — design.md §3.4.1.
 *
 * A centered single card (`max-w-sm`) on the app background with the brand mark +
 * tagline above it. No nav. Used by the Login page. Identical across breakpoints:
 * the card stays centered and goes full-width-minus-gutter on phones (`px-4`).
 *
 * This is a server component (no client state) — the page it wraps owns any
 * interactivity. A skip-to-content link is the first focusable element so the
 * keyboard path is correct even on this minimal screen (design.md §4.8).
 */

import { getTranslations } from "next-intl/server";
import { BrandMark } from "@/components/organisms";
import { cn } from "@/lib/utils";

export interface AuthLayoutProps {
  children: React.ReactNode;
  className?: string;
}

export async function AuthLayout({ children, className }: AuthLayoutProps) {
  const t = await getTranslations();

  return (
    <div
      className={cn(
        "flex min-h-dvh flex-col items-center justify-center bg-background px-4 py-12",
        className
      )}
    >
      {/* Skip link — first focusable, visually hidden until focused (§4.8). */}
      <a
        href="#auth-content"
        className="sr-only focus:not-sr-only focus:fixed focus:top-4 focus:left-4 focus:z-50 focus:rounded-md focus:bg-card focus:px-3 focus:py-2 focus:text-sm focus:shadow-md focus:ring-2 focus:ring-ring focus:outline-none"
      >
        {t("nav.skipToContent")}
      </a>

      {/* Brand mark + tagline above the card. */}
      <div className="mb-8 flex flex-col items-center gap-3 text-center">
        <BrandMark className="size-10" />
        <div className="space-y-1">
          <p className="text-2xl font-semibold text-foreground">
            {t("app.name")}
          </p>
          <p className="text-sm text-muted-foreground">{t("app.tagline")}</p>
        </div>
      </div>

      {/* The auth card slot. */}
      <main
        id="auth-content"
        className="w-full max-w-sm rounded-xl border border-border bg-card p-6 shadow-sm"
      >
        {children}
      </main>
    </div>
  );
}
