"use client";

/**
 * CommandPalette (organism) — design.md §3.3 / §4.8. The ⌘K power-user surface
 * (cmdk in a Dialog): jump to any of the caller's sites, run global actions
 * (new site, dashboard, admin*), and switch theme — all fully keyboard-operable.
 *
 * Controlled by the parent (TopNav owns open-state) via `open`/`onOpenChange`
 * (base-ui Dialog Root semantics). It ALSO registers the global ⌘K / Ctrl-K
 * shortcut itself so the palette opens from anywhere, and closes after running
 * an action. Navigation uses the client router (next/navigation).
 */

import { useCallback, useEffect } from "react";
import { useRouter } from "next/navigation";
import { useTheme } from "next-themes";
import { useTranslations } from "next-intl";
import {
  LayoutGrid,
  Monitor,
  Moon,
  Plus,
  Shield,
  Sun,
} from "lucide-react";
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
  CommandShortcut,
} from "@/components/ui/command";
import { StatusBadge } from "@/components/atoms";
import { useMe, useSites } from "@/lib/api/hooks";

export interface CommandPaletteProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function CommandPalette({ open, onOpenChange }: CommandPaletteProps) {
  const t = useTranslations();
  const router = useRouter();
  const { setTheme } = useTheme();
  const { data: me } = useMe();
  const { data: sites } = useSites();

  // Global ⌘K / Ctrl-K toggles the palette from anywhere (design.md §4.8).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() === "k" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        onOpenChange(!open);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onOpenChange]);

  // Run an action then dismiss; keeps every item's handler uniform.
  const run = useCallback(
    (action: () => void) => {
      action();
      onOpenChange(false);
    },
    [onOpenChange]
  );

  return (
    <CommandDialog
      open={open}
      onOpenChange={onOpenChange}
      title={t("nav.commandPalette")}
      description={t("common.search")}
    >
      <CommandInput placeholder={t("common.search")} />
      <CommandList>
        <CommandEmpty>{t("errors.notFound")}</CommandEmpty>

        {/* Global actions. */}
        <CommandGroup heading={t("nav.dashboard")}>
          <CommandItem
            value="new-site create"
            onSelect={() => run(() => router.push("/sites/new"))}
          >
            <Plus aria-hidden="true" />
            {t("nav.newSite")}
            <CommandShortcut>N</CommandShortcut>
          </CommandItem>
          <CommandItem
            value="dashboard home"
            onSelect={() => run(() => router.push("/dashboard"))}
          >
            <LayoutGrid aria-hidden="true" />
            {t("nav.dashboard")}
          </CommandItem>
          {me?.user.isAdmin ? (
            <CommandItem
              value="admin"
              onSelect={() => run(() => router.push("/admin"))}
            >
              <Shield aria-hidden="true" />
              {t("admin.title")}
            </CommandItem>
          ) : null}
        </CommandGroup>

        {/* Jump to a site. */}
        {sites && sites.length > 0 ? (
          <>
            <CommandSeparator />
            <CommandGroup heading={t("dashboard.title")}>
              {sites.map((site) => (
                <CommandItem
                  key={site.id}
                  value={`site ${site.handle} ${site.description ?? ""}`}
                  onSelect={() =>
                    run(() => router.push(`/sites/${site.handle}`))
                  }
                >
                  <span className="truncate">{site.handle}</span>
                  <StatusBadge
                    status={site.hasPublished ? "published" : "draft"}
                    className="ml-auto"
                  />
                </CommandItem>
              ))}
            </CommandGroup>
          </>
        ) : null}

        {/* Theme. */}
        <CommandSeparator />
        <CommandGroup heading={t("theme.label")}>
          <CommandItem
            value="theme light"
            onSelect={() => run(() => setTheme("light"))}
          >
            <Sun aria-hidden="true" />
            {t("theme.light")}
          </CommandItem>
          <CommandItem
            value="theme dark"
            onSelect={() => run(() => setTheme("dark"))}
          >
            <Moon aria-hidden="true" />
            {t("theme.dark")}
          </CommandItem>
          <CommandItem
            value="theme system"
            onSelect={() => run(() => setTheme("system"))}
          >
            <Monitor aria-hidden="true" />
            {t("theme.system")}
          </CommandItem>
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  );
}
