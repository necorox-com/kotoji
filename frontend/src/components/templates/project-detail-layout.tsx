"use client";

/**
 * ProjectDetailLayout (template) — design.md §3.4.3 / §3.5 ProjectDetail.
 *
 * The shared chrome for every ProjectDetail section (Files/Editor · Branches ·
 * Publish · History · Members · Settings). The six sections are URL-addressable
 * NESTED ROUTES (design.md §3.5 recommendation), so this template renders:
 *  - the standard TopNav with a "ダッシュボード / {handle}" breadcrumb,
 *  - the sticky BranchBar (version picker + preview URL + quick publish),
 *  - a section TabBar implemented as nav LINKS to the nested routes (so each tab
 *    is deep-linkable + SSR-prefetchable); horizontally scrollable on phones,
 *  - the active section's content below.
 *
 * Responsive collapse (design.md §3.4.3 "collapse panes → tabs at < lg"):
 *  - the split-pane (FileTree | Editor) is NOT this template's concern — it lives
 *    in the Files page, which renders the desktop split / tablet tabs / phone
 *    drawer itself. This template provides the section-level tabs that are shown
 *    at every breakpoint (they ARE the tablet/phone navigation model, and double
 *    as the desktop section switcher since we use nested routes rather than an
 *    xl: right rail to keep one consistent, addressable model).
 *
 * Active "version" (branch) is carried in the `?branch=` search param so it
 * survives navigation BETWEEN sections (the History/Publish/Branches tabs all
 * read the same selection). BranchBar changes update that param.
 */

import { useCallback, useMemo } from "react";
import NextLink from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useTranslations } from "next-intl";
import {
  AppSidebar,
  BranchBar,
  TopNav,
} from "@/components/organisms";
import { useConfig } from "@/lib/api/hooks";
import { useSiteRole } from "@/hooks";
import type { PublishMode, SiteRole } from "@/lib/api/types";
import { cn } from "@/lib/utils";

/** The default "version" when none is selected (CANONICAL §5.2 logical draft). */
const DEFAULT_BRANCH = "draft";
const DEFAULT_BASE_DOMAIN = "hosting.example.com";

/** The six ProjectDetail sections, in tab order (design.md §3.5). */
type SectionId =
  | "files"
  | "branches"
  | "publish"
  | "history"
  | "members"
  | "settings";

interface SectionDef {
  id: SectionId;
  /** Path suffix relative to /sites/{handle} ("" = the default Files route). */
  suffix: string;
}

const SECTIONS: SectionDef[] = [
  { id: "files", suffix: "" },
  { id: "branches", suffix: "/branches" },
  { id: "publish", suffix: "/publish" },
  { id: "history", suffix: "/history" },
  { id: "members", suffix: "/members" },
  { id: "settings", suffix: "/settings" },
];

export interface ProjectDetailLayoutProps {
  handle: string;
  children: React.ReactNode;
  /**
   * Force the content area full-bleed (no max-width/gutter wrapper). By default
   * the Files section is full-bleed automatically (its split-pane needs all the
   * width) and every other section uses the framed, centered reading column.
   */
  fullBleedContent?: boolean;
  className?: string;
}

export function ProjectDetailLayout({
  handle,
  children,
  fullBleedContent,
  className,
}: ProjectDetailLayoutProps) {
  const t = useTranslations();
  const tTabs = useTranslations("tabs");
  const tNav = useTranslations("nav");
  const router = useRouter();
  const pathname = usePathname() ?? "";
  const searchParams = useSearchParams();

  const { data: config } = useConfig();
  const { role } = useSiteRole(handle);

  const baseDomain = config?.baseDomain ?? DEFAULT_BASE_DOMAIN;
  const publishMode: PublishMode = config?.defaultPublishMode ?? "direct";

  // Active branch from the URL (shared across sections), default to draft.
  const branch = searchParams.get("branch") ?? DEFAULT_BRANCH;

  // Preserve the active branch across section navigations.
  const sectionHref = useCallback(
    (suffix: string) => {
      const base = `/sites/${handle}${suffix}`;
      return branch !== DEFAULT_BRANCH
        ? `${base}?branch=${encodeURIComponent(branch)}`
        : base;
    },
    [handle, branch]
  );

  // Which section is active (longest-matching suffix wins so /history doesn't
  // also light up Files). The default route ("") matches exactly /sites/{handle}.
  const base = `/sites/${handle}`;
  const activeSection: SectionId = useMemo(() => {
    const rest = pathname.startsWith(base) ? pathname.slice(base.length) : "";
    const match = SECTIONS.filter((s) => s.suffix && rest.startsWith(s.suffix))
      .sort((a, b) => b.suffix.length - a.suffix.length)[0];
    return match?.id ?? "files";
  }, [pathname, base]);

  // Change the active version: stay on the current section, swap the param.
  const onBranchChange = useCallback(
    (next: string) => {
      const active = SECTIONS.find((s) => s.id === activeSection);
      const target = `${base}${active?.suffix ?? ""}`;
      router.push(
        next !== DEFAULT_BRANCH
          ? `${target}?branch=${encodeURIComponent(next)}`
          : target
      );
    },
    [router, base, activeSection]
  );

  // Quick publish from the BranchBar → the Publish section (the real flow).
  const onPublish = useCallback(() => {
    router.push(sectionHref("/publish"));
  }, [router, sectionHref]);

  // Files is full-bleed by default (split-pane needs the width); an explicit
  // prop overrides. Computed once so the JSX stays readable.
  const isFullBleed = fullBleedContent ?? activeSection === "files";

  const breadcrumbs = [
    { label: tNav("dashboard"), href: "/dashboard" },
    { label: handle },
  ];

  return (
    <div className={cn("flex min-h-dvh flex-col bg-background", className)}>
      <a
        href="#project-content"
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-[60] focus:rounded-md focus:bg-card focus:px-3 focus:py-2 focus:text-sm focus:shadow-md focus:ring-2 focus:ring-ring focus:outline-none"
      >
        {t("nav.skipToContent")}
      </a>

      <TopNav breadcrumbs={breadcrumbs} />

      <div className="flex min-h-0 flex-1">
        <AppSidebar />

        <div className="flex min-w-0 flex-1 flex-col">
          {/* Version bar (sticky); quick-publish jumps to the Publish section. */}
          <BranchBar
            handle={handle}
            branch={branch}
            onBranchChange={onBranchChange}
            role={role as SiteRole}
            baseDomain={baseDomain}
            onPublish={onPublish}
            publishMode={publishMode}
          />

          {/* Section tabs — nav links to the nested routes; scroll on phones. */}
          <nav
            aria-label={t("nav.menu")}
            className="sticky top-0 z-10 flex gap-1 overflow-x-auto border-b border-border bg-background/95 px-2 backdrop-blur supports-[backdrop-filter]:bg-background/80"
          >
            {SECTIONS.map((s) => {
              const active = s.id === activeSection;
              return (
                <NextLink
                  key={s.id}
                  href={sectionHref(s.suffix)}
                  aria-current={active ? "page" : undefined}
                  className={cn(
                    "shrink-0 border-b-2 px-3 py-2.5 text-sm font-medium whitespace-nowrap transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
                    active
                      ? "border-primary text-foreground"
                      : "border-transparent text-muted-foreground hover:text-foreground"
                  )}
                >
                  {tTabs(s.id)}
                </NextLink>
              );
            })}
          </nav>

          {/* The active section. Files is full-bleed (split-pane needs width);
              other sections use a centered reading column. */}
          <main id="project-content" className="min-h-0 flex-1">
            {isFullBleed ? (
              children
            ) : (
              <div className="mx-auto w-full max-w-screen-xl px-4 py-6 sm:px-6 lg:px-8">
                {children}
              </div>
            )}
          </main>
        </div>
      </div>
    </div>
  );
}
