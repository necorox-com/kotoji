"use client";

/**
 * TopNav (organism) — design.md §3.3/§3.4.2. The sticky app header (`z-nav`):
 * brand mark + wordmark, a breadcrumb region, the global search/command trigger
 * (⌘K → CommandPalette), a theme toggle, and the UserMenu. On phone it collapses
 * to: hamburger (opens AppSidebar as a Sheet) + logo + UserMenu, with the search
 * trigger shrinking to an icon. TopNav owns the open-state for the mobile sidebar
 * Sheet and the CommandPalette so a single header instance drives both.
 *
 * The koto-bridge "人"-glyph (`BrandMark`) is exported for reuse (AuthLayout,
 * sidebar header) — design.md §1.1 brand mark.
 */

import { useState, useSyncExternalStore } from "react";
import NextLink from "next/link";
import { useTheme } from "next-themes";
import { useTranslations } from "next-intl";
import { Menu, Monitor, Moon, Search, Sun } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Kbd } from "@/components/atoms";
import { Breadcrumbs, type BreadcrumbCrumb } from "./breadcrumbs";
import { UserMenu } from "./user-menu";
import { CommandPalette } from "./command-palette";
import { AppSidebarSheet } from "./app-sidebar";
import { cn } from "@/lib/utils";

/**
 * useMounted — true only after client mount. Uses useSyncExternalStore (the
 * React 19 idiom in this codebase, see hooks/use-media-query) so we read a
 * post-hydration value WITHOUT setState-in-effect; the server snapshot is false,
 * the client snapshot is true. Lets theme controls render hydration-safe (§4.4).
 */
function useMounted(): boolean {
  return useSyncExternalStore(
    () => () => {},
    () => true,
    () => false
  );
}

/** The koto-bridge "人"-shaped brand glyph (§1.1). Gold string accent. */
export function BrandMark({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 56 56"
      fill="none"
      aria-hidden="true"
      className={cn("size-6 text-primary", className)}
    >
      <path
        d="M28 10 L44 44 M28 10 L12 44"
        stroke="currentColor"
        strokeWidth="3"
        strokeLinecap="round"
      />
      <path
        d="M18 34 L38 34"
        stroke="var(--brand-gold)"
        strokeWidth="3"
        strokeLinecap="round"
      />
    </svg>
  );
}

/** Compact theme switcher used in the header (full control in UserMenu too). */
function ThemeToggle() {
  const t = useTranslations("theme");
  const { theme, setTheme } = useTheme();
  const mounted = useMounted();

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button variant="ghost" size="icon" aria-label={t("toggle")} />
        }
      >
        {/* Both icons rendered; CSS shows the one matching the resolved theme to
            avoid a hydration mismatch flash (only the dark icon shows in dark). */}
        <Sun className="dark:hidden" aria-hidden="true" />
        <Moon className="hidden dark:block" aria-hidden="true" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-40">
        <DropdownMenuRadioGroup
          value={mounted ? (theme ?? "system") : undefined}
          onValueChange={setTheme}
        >
          <DropdownMenuRadioItem value="light">
            <Sun aria-hidden="true" />
            {t("light")}
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="dark">
            <Moon aria-hidden="true" />
            {t("dark")}
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="system">
            <Monitor aria-hidden="true" />
            {t("system")}
          </DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

export interface TopNavProps {
  /** Breadcrumb trail for the current route (rendered next to the logo). */
  breadcrumbs?: BreadcrumbCrumb[];
  className?: string;
}

export function TopNav({ breadcrumbs, className }: TopNavProps) {
  const t = useTranslations();
  // TopNav owns both overlays so the header is the single source of truth.
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);

  return (
    <header
      className={cn(
        "sticky top-0 z-20 flex h-14 items-center gap-2 border-b border-border bg-background/95 px-4 backdrop-blur supports-[backdrop-filter]:bg-background/80 sm:px-6",
        className
      )}
    >
      {/* Mobile hamburger — opens the sidebar drawer (hidden on desktop rail). */}
      <Button
        variant="ghost"
        size="icon"
        aria-label={t("nav.menu")}
        onClick={() => setSidebarOpen(true)}
        className="lg:hidden"
      >
        <Menu aria-hidden="true" />
      </Button>

      {/* Brand mark + wordmark links home; wordmark hidden on the smallest phones. */}
      <NextLink
        href="/dashboard"
        className="flex shrink-0 items-center gap-2 rounded-md focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        aria-label={t("app.name")}
      >
        <BrandMark />
        <span className="hidden text-base font-semibold text-foreground sm:inline">
          {t("app.name")}
        </span>
      </NextLink>

      {/* Breadcrumb region — collapses gracefully; hidden when there's no trail. */}
      {breadcrumbs && breadcrumbs.length > 0 ? (
        <div className="ml-2 hidden min-w-0 md:flex">
          <Breadcrumbs items={breadcrumbs} />
        </div>
      ) : null}

      {/* Spacer pushes the actions to the trailing edge. */}
      <div className="flex-1" />

      {/* Global search / command trigger. Full pill on ≥sm, icon on phone. */}
      <Button
        variant="outline"
        onClick={() => setPaletteOpen(true)}
        aria-label={t("nav.commandPalette")}
        aria-keyshortcuts="Meta+K Control+K"
        className="hidden h-8 min-w-44 justify-start gap-2 text-muted-foreground sm:inline-flex"
      >
        <Search aria-hidden="true" />
        <span className="flex-1 text-left">{t("common.search")}</span>
        <Kbd aria-hidden="true">⌘K</Kbd>
      </Button>
      <Button
        variant="ghost"
        size="icon"
        onClick={() => setPaletteOpen(true)}
        aria-label={t("nav.commandPalette")}
        className="sm:hidden"
      >
        <Search aria-hidden="true" />
      </Button>

      <ThemeToggle />
      <UserMenu />

      {/* Overlays driven by this header's state. */}
      <AppSidebarSheet open={sidebarOpen} onOpenChange={setSidebarOpen} />
      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} />
    </header>
  );
}
