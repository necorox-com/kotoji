"use client";

/**
 * UserMenu (organism) — design.md §3.2/§3.3/§4.3. Avatar trigger → dropdown with
 * the signed-in identity, a theme switcher (light/dark/system, next-themes), a
 * project-settings shortcut (when inside a project), the Admin link for instance
 * superusers (CANONICAL §6 `is_admin`), and Sign out.
 *
 * Sign out POSTs to the backend logout endpoint (it is POST-only — a GET 405s)
 * and then hard-navigates to /login so the server clears the session cookie and
 * all client/TanStack state is dropped. Navigation items use onClick +
 * router.push (not a render-prop anchor) so they compose cleanly with base-ui's
 * menu items.
 *
 * Data: reads the session via useMe(); while it resolves we show a quiet avatar
 * skeleton so the nav never jumps. Theme controls are hydration-guarded.
 */

import { useSyncExternalStore } from "react";
import { usePathname, useRouter } from "next/navigation";
import { useTheme } from "next-themes";
import { useTranslations } from "next-intl";
import { LogOut, Monitor, Moon, Settings, Shield, Sun } from "lucide-react";
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
import { CSRF_COOKIE, CSRF_HEADER, readCookie } from "@/lib/api/client";
import type { User } from "@/lib/api/types";
import { cn } from "@/lib/utils";

const API_BASE = process.env.NEXT_PUBLIC_API_BASE_URL ?? "";

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

/**
 * Sign out via POST (the backend route is POST-only; a GET 405s and shows an
 * error page). The server clears the session cookie; we then hard-navigate to
 * /login so the client cache is dropped. Best-effort — we leave for /login even
 * if the request fails. /auth/logout is outside the /api CSRF subtree, so the
 * token is only sent opportunistically.
 */
async function logout(): Promise<void> {
  try {
    const csrf = readCookie(`__Host-${CSRF_COOKIE}`) || readCookie(CSRF_COOKIE);
    await fetch(`${API_BASE}/auth/logout`, {
      method: "POST",
      credentials: "include",
      headers: csrf ? { [CSRF_HEADER]: csrf } : undefined,
    });
  } catch {
    /* ignore — fall through to the login page regardless */
  }
  window.location.assign("/login");
}

export interface UserMenuProps {
  /** Optional menu side; defaults to a bottom-end placement under the avatar. */
  align?: "start" | "center" | "end";
  className?: string;
}

export function UserMenu({ align = "end", className }: UserMenuProps) {
  const t = useTranslations();
  const router = useRouter();
  const pathname = usePathname();
  const { data: me, isLoading } = useMe();
  const { theme, setTheme } = useTheme();

  // Hydration guard: next-themes' resolved value is only correct after mount.
  const mounted = useMounted();

  // Current project handle, when inside a project — powers the settings shortcut.
  // Excludes the /sites/new create route.
  const handleMatch = pathname?.match(/^\/sites\/([^/]+)/);
  const projectHandle =
    handleMatch && handleMatch[1] !== "new" ? handleMatch[1] : null;

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

        {/* Project settings shortcut (only when inside a project). */}
        {projectHandle ? (
          <DropdownMenuItem
            onClick={() => router.push(`/sites/${projectHandle}/settings`)}
          >
            <Settings aria-hidden="true" />
            {t("tabs.settings")}
          </DropdownMenuItem>
        ) : null}

        {/* Admin link only for instance superusers (CANONICAL §6). */}
        {user.isAdmin ? (
          <DropdownMenuItem onClick={() => router.push("/admin")}>
            <Shield aria-hidden="true" />
            {t("admin.title")}
          </DropdownMenuItem>
        ) : null}

        <DropdownMenuSeparator />

        {/* Sign out — POST logout, then hard-navigate to /login. */}
        <DropdownMenuItem variant="destructive" onClick={() => void logout()}>
          <LogOut aria-hidden="true" />
          {t("nav.signOut")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
