"use client";

/**
 * AppSidebar (organism) — design.md §3.3/§3.4.2. The primary app navigation:
 * Dashboard, Admin (instance superusers only — CANONICAL §6), a "Recent" list
 * of the caller's sites, the "New site" CTA, and a Settings link pinned to the
 * bottom (instance/account settings, /settings). Two presentations share one
 * inner body (`SidebarNav`):
 *  - desktop (`lg:`): a persistent left rail rendered by DashboardLayout,
 *  - phone/tablet: a Radix-style Sheet drawer opened from the TopNav hamburger
 *    (`AppSidebarSheet`, controlled via open/onOpenChange — base-ui Dialog Root).
 *
 * Active route is highlighted from usePathname (accent tint). Recent sites come
 * from useSites(); a small slice keeps the rail short. Loading shows skeletons,
 * empty/error stay quiet (the Dashboard itself surfaces the real empty/error).
 */

import { usePathname } from "next/navigation";
import NextLink from "next/link";
import { useTranslations } from "next-intl";
import { LayoutGrid, Plus, Settings, Shield } from "lucide-react";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { BrandMark } from "./top-nav";
import { useMe, useSites } from "@/lib/api/hooks";
import { cn } from "@/lib/utils";

/** How many recent sites to surface in the rail (keeps it scannable). */
const RECENT_LIMIT = 5;

/** A single nav row; active when its href matches the current path prefix. */
function NavLink({
  href,
  label,
  icon: Icon,
  active,
  onNavigate,
}: {
  href: string;
  label: string;
  icon: React.ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  active: boolean;
  onNavigate?: () => void;
}) {
  return (
    <NextLink
      href={href}
      onClick={onNavigate}
      aria-current={active ? "page" : undefined}
      className={cn(
        "flex items-center gap-2.5 rounded-md px-2.5 py-2 text-sm font-medium transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
        active
          ? "bg-accent text-accent-foreground"
          : "text-foreground hover:bg-muted"
      )}
    >
      <Icon className="size-[18px] shrink-0" aria-hidden={true} />
      <span className="truncate">{label}</span>
    </NextLink>
  );
}

export interface SidebarNavProps {
  /** Called after any nav item is clicked (lets the Sheet close itself). */
  onNavigate?: () => void;
  className?: string;
}

/** The shared sidebar body used by both the rail and the drawer. */
export function SidebarNav({ onNavigate, className }: SidebarNavProps) {
  const t = useTranslations();
  const pathname = usePathname() ?? "";
  const { data: me } = useMe();
  const { data: sites, isLoading } = useSites();

  const isActive = (href: string) =>
    href === "/dashboard"
      ? pathname === "/dashboard"
      : pathname === href || pathname.startsWith(`${href}/`);

  // Most-recently-updated sites first, capped for a tidy rail.
  const recent = (sites ?? [])
    .slice()
    .sort(
      (a, b) =>
        new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime()
    )
    .slice(0, RECENT_LIMIT);

  return (
    <nav
      aria-label={t("nav.menu")}
      className={cn("flex h-full flex-col gap-4 p-3", className)}
    >
      {/* New site — primary CTA, always reachable at the top. */}
      <Button
        variant="default"
        render={<NextLink href="/sites/new" onClick={onNavigate} />}
        className="justify-start"
      >
        <Plus aria-hidden="true" />
        {t("nav.newSite")}
      </Button>

      <div className="flex flex-col gap-0.5">
        <NavLink
          href="/dashboard"
          label={t("nav.dashboard")}
          icon={LayoutGrid}
          active={isActive("/dashboard")}
          onNavigate={onNavigate}
        />
        {me?.user.isAdmin ? (
          <NavLink
            href="/admin"
            label={t("nav.admin")}
            icon={Shield}
            active={isActive("/admin")}
            onNavigate={onNavigate}
          />
        ) : null}
      </div>

      {/* Recent sites — quiet section header + linked rows. */}
      <div className="min-h-0 flex-1 overflow-y-auto">
        <p className="px-2.5 pb-1.5 text-xs font-medium tracking-wide text-muted-foreground uppercase">
          {t("nav.recent")}
        </p>
        <div className="flex flex-col gap-0.5">
          {isLoading ? (
            <>
              <Skeleton className="h-8 w-full rounded-md" />
              <Skeleton className="h-8 w-full rounded-md" />
              <Skeleton className="h-8 w-full rounded-md" />
            </>
          ) : recent.length > 0 ? (
            recent.map((site) => {
              const href = `/sites/${site.handle}`;
              return (
                <NextLink
                  key={site.id}
                  href={href}
                  onClick={onNavigate}
                  aria-current={isActive(href) ? "page" : undefined}
                  className={cn(
                    "truncate rounded-md px-2.5 py-1.5 text-sm transition-colors focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
                    isActive(href)
                      ? "bg-accent text-accent-foreground"
                      : "text-muted-foreground hover:bg-muted hover:text-foreground"
                  )}
                  title={site.handle}
                >
                  {site.handle}
                </NextLink>
              );
            })
          ) : (
            <p className="px-2.5 py-1.5 text-xs text-muted-foreground">
              {t("dashboard.empty.title")}
            </p>
          )}
        </div>
      </div>

      {/* Instance/account settings — pinned to the bottom of the rail. The
          Recent section above is `flex-1`, so `mt-auto` keeps this row anchored
          even when the list is short. */}
      <div className="mt-auto border-t border-border pt-3">
        <NavLink
          href="/settings"
          label={t("settings.title")}
          icon={Settings}
          active={isActive("/settings")}
          onNavigate={onNavigate}
        />
      </div>
    </nav>
  );
}

/** Persistent desktop rail (rendered by DashboardLayout at `lg:`). */
export function AppSidebar({ className }: { className?: string }) {
  return (
    <aside
      className={cn(
        "hidden w-64 shrink-0 border-r border-border bg-card lg:block",
        className
      )}
    >
      <SidebarNav />
    </aside>
  );
}

export interface AppSidebarSheetProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/** Mobile/tablet drawer variant opened from the TopNav hamburger. */
export function AppSidebarSheet({ open, onOpenChange }: AppSidebarSheetProps) {
  const t = useTranslations();
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="left" className="w-72 p-0">
        <SheetHeader className="border-b border-border">
          <SheetTitle className="flex items-center gap-2">
            <BrandMark className="size-5" />
            {t("app.name")}
          </SheetTitle>
        </SheetHeader>
        {/* Selecting any item closes the drawer for a phone-native feel. */}
        <SidebarNav onNavigate={() => onOpenChange(false)} />
      </SheetContent>
    </Sheet>
  );
}
