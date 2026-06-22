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

/**
 * kotoji-tōrō (琴柱灯籠) brand mark — the same lantern as the favicon
 * (app/icon.svg): curved cap + finial, a glowing light chamber, and the two
 * splayed koto-bridge legs, on the Kaga-indigo→violet brand tile.
 */
export function BrandMark({ className }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 32 32"
      role="img"
      aria-label="kotoji"
      className={cn("size-6", className)}
    >
      <defs>
        <linearGradient id="kotoji-mark-bg" x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#5b5bf0" />
          <stop offset=".55" stopColor="#7a5bf5" />
          <stop offset="1" stopColor="#9b6bff" />
        </linearGradient>
        <radialGradient id="kotoji-mark-glow" cx=".5" cy=".5" r=".5">
          <stop offset="0" stopColor="#ffe6a8" />
          <stop offset="1" stopColor="#ffcf6b" />
        </radialGradient>
      </defs>
      <rect width="32" height="32" rx="7.5" fill="url(#kotoji-mark-bg)" />
      <g stroke="#fff" strokeWidth="2.2" strokeLinecap="round">
        <path d="M12.8 18.7 8.2 26.3" />
        <path d="M19.2 18.7 23.8 26.3" />
      </g>
      <path
        d="M6 11.7 Q9 10.8 10.8 11.1 Q16 6.7 21.2 11.1 Q23 10.8 26 11.7 Q21 8.2 16 7 Q11 8.2 6 11.7 Z"
        fill="#fff"
      />
      <circle cx="16" cy="5.4" r="1.5" fill="#fff" />
      <rect x="10.7" y="12" width="10.6" height="7.4" rx="2.4" fill="#fff" />
      <circle cx="16" cy="15.7" r="2.2" fill="url(#kotoji-mark-glow)" />
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
