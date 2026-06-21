"use client";

/**
 * UserMenu (organism) — design.md §3.2/§3.3/§4.3. Avatar trigger → dropdown with
 * the signed-in identity, a theme switcher (light/dark/system, next-themes), the
 * Admin link when the user is an instance superuser (CANONICAL §6 `is_admin`),
 * and Sign out. Sign out is a FULL navigation to the backend logout endpoint
 * (clears the server session + cookie, §4.3) — not a client route push.
 *
 * Data: reads the session via useMe(); while it resolves we show a quiet avatar
 * skeleton so the nav never jumps. Theme controls are hydration-guarded (mounted
 * flag) so SSR/CSR markup match (§4.4).
 */

import { useSyncExternalStore } from "react";
import { useTheme } from "next-themes";
import { useTranslations } from "next-intl";
import { LogOut, Monitor, Moon, Shield, Sun } from "lucide-react";
import NextLink from "next/link";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useMe } from "@/lib/api/hooks";
import type { User } from "@/lib/api/types";
import { cn } from "@/lib/utils";

/**
 * useMounted — true only after client mount, via useSyncExternalStore (the
 * codebase idiom; avoids setState-in-effect). Makes the theme radio render a
 * hydration-safe value (§4.4).
 */
function useMounted(): boolean {
  return useSyncExternalStore(
    () => () => {},
    () => true,
    () => false
  );
}

/** Build up-to-two-letter initials from a display name or email local-part. */
function initialsOf(user: Pick<User, "displayName" | "email">): string {
  const source = user.displayName?.trim() || user.email.split("@")[0] || "";
  const parts = source.split(/[\s._-]+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

/** Absolute URL to the backend logout (honors the configured API base). */
function logoutUrl(): string {
  const base = process.env.NEXT_PUBLIC_API_BASE_URL ?? "";
  return `${base}/auth/logout`;
}

export interface UserMenuProps {
  /** Optional menu side; defaults to a bottom-end placement under the avatar. */
  align?: "start" | "center" | "end";
  className?: string;
}

export function UserMenu({ align = "end", className }: UserMenuProps) {
  const t = useTranslations();
  const { data: me, isLoading } = useMe();
  const { theme, setTheme } = useTheme();

  // Hydration guard: next-themes' resolved value is only correct after mount.
  const mounted = useMounted();

  if (isLoading && !me) {
    return <Skeleton className={cn("size-8 rounded-full", className)} />;
  }
  if (!me) return null;

  const { user } = me;
  const initials = initialsOf(user);

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="icon"
            aria-label={t("nav.userMenu")}
            className={cn("rounded-full", className)}
          />
        }
      >
        <Avatar size="sm">
          {user.avatarUrl ? (
            <AvatarImage src={user.avatarUrl} alt="" />
          ) : null}
          <AvatarFallback className="bg-accent text-accent-foreground">
            {initials}
          </AvatarFallback>
        </Avatar>
      </DropdownMenuTrigger>

      <DropdownMenuContent align={align} className="w-60">
        {/* Identity block — name + email, both truncated to keep width bounded. */}
        <div className="flex items-center gap-2 px-1.5 py-1.5">
          <Avatar size="sm">
            {user.avatarUrl ? <AvatarImage src={user.avatarUrl} alt="" /> : null}
            <AvatarFallback className="bg-accent text-accent-foreground">
              {initials}
            </AvatarFallback>
          </Avatar>
          <div className="min-w-0">
            <p className="truncate text-sm font-medium text-foreground">
              {user.displayName}
            </p>
            <p className="truncate text-xs text-muted-foreground">
              {user.email}
            </p>
          </div>
        </div>

        <DropdownMenuSeparator />

        {/* Theme switcher (radio group; hydration-safe value). */}
        <DropdownMenuLabel>{t("theme.label")}</DropdownMenuLabel>
        <DropdownMenuRadioGroup
          value={mounted ? (theme ?? "system") : undefined}
          onValueChange={setTheme}
        >
          <DropdownMenuRadioItem value="light">
            <Sun aria-hidden="true" />
            {t("theme.light")}
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="dark">
            <Moon aria-hidden="true" />
            {t("theme.dark")}
          </DropdownMenuRadioItem>
          <DropdownMenuRadioItem value="system">
            <Monitor aria-hidden="true" />
            {t("theme.system")}
          </DropdownMenuRadioItem>
        </DropdownMenuRadioGroup>

        <DropdownMenuSeparator />

        {/* Admin link only for instance superusers (CANONICAL §6). */}
        {user.isAdmin ? (
          <DropdownMenuItem render={<NextLink href="/admin" />}>
            <Shield aria-hidden="true" />
            {t("admin.title")}
          </DropdownMenuItem>
        ) : null}

        {/* Sign out — full navigation so the backend clears the session cookie.
            (aria-label on the anchor: the DropdownMenuItem injects the visible
            label as children at runtime, but it satisfies a11y lint here too.) */}
        <DropdownMenuItem
          variant="destructive"
          render={<a href={logoutUrl()} aria-label={t("nav.signOut")} />}
        >
          <LogOut aria-hidden="true" />
          {t("nav.signOut")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
