"use client";

/**
 * DashboardLayout (template) — design.md §3.4.2.
 *
 * The standard authenticated app chrome used by Dashboard, CreateSite, and Admin:
 *  - desktop (`lg:`): a persistent AppSidebar (left rail) + TopNav (top) + a
 *    scrollable content column constrained to `max-w-screen-xl` with responsive
 *    gutters,
 *  - tablet/phone (`< lg`): TopNav only; the AppSidebar becomes a Sheet drawer
 *    opened from the TopNav hamburger (TopNav owns that drawer state), and the
 *    content goes single-column with `px-4`.
 *
 * A skip-to-content link is the first focusable element; the content region is a
 * proper `<main>` landmark (design.md §4.8 landmarks + skip link).
 *
 * Optional `breadcrumbs` are threaded into TopNav. `fullBleed` removes the
 * max-width/gutter wrapper for screens that need all the width (not used by the
 * three dashboard pages, but kept so AdminLayout can compose this without a
 * second scroll container).
 */

import { useTranslations } from "next-intl";
import { AppSidebar, TopNav } from "@/components/organisms";
import type { BreadcrumbCrumb } from "@/components/organisms";
import { cn } from "@/lib/utils";

export interface DashboardLayoutProps {
  children: React.ReactNode;
  /** Breadcrumb trail rendered in the TopNav. */
  breadcrumbs?: BreadcrumbCrumb[];
  /** Drop the centered max-width container (full-width content). */
  fullBleed?: boolean;
  className?: string;
}

export function DashboardLayout({
  children,
  breadcrumbs,
  fullBleed,
  className,
}: DashboardLayoutProps) {
  const t = useTranslations();

  return (
    <div className={cn("flex min-h-dvh flex-col bg-background", className)}>
      {/* Skip link — first focusable; jumps over nav to the content (§4.8). */}
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-[60] focus:rounded-md focus:bg-card focus:px-3 focus:py-2 focus:text-sm focus:shadow-md focus:ring-2 focus:ring-ring focus:outline-none"
      >
        {t("nav.skipToContent")}
      </a>

      <TopNav breadcrumbs={breadcrumbs} />

      {/* Rail + content. The rail is hidden < lg (AppSidebar handles that); the
          mobile drawer is rendered inside TopNav, so nothing extra here. */}
      <div className="flex min-h-0 flex-1">
        <AppSidebar />
        <main id="main-content" className="min-w-0 flex-1">
          {fullBleed ? (
            children
          ) : (
            <div className="mx-auto w-full max-w-screen-xl px-4 py-6 sm:px-6 lg:px-8 lg:py-8">
              {children}
            </div>
          )}
        </main>
      </div>
    </div>
  );
}
