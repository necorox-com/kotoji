"use client";

/**
 * AdminLayout (template) — design.md §3.4.4.
 *
 * The same chrome as DashboardLayout plus a secondary admin sub-nav (Users /
 * Projects / Quotas / Reserved words). Per design: a secondary TabBar on desktop,
 * a Select on phone (the four labels would crowd a 375px bar). The active section
 * is owned by the parent (URL-addressable in a fuller build); here it is driven
 * by `section` + `onSectionChange` so the Admin page stays the single source of
 * truth for which panel shows.
 *
 * Admin-guarding is the (app) route guard's job (useRequireAuth requireAdmin);
 * this template is purely presentational.
 */

import { useTranslations } from "next-intl";
import { DashboardLayout } from "./dashboard-layout";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { SectionHeading } from "@/components/atoms";
import { cn } from "@/lib/utils";

/** The four admin sections (design.md §3.5 Admin). */
export type AdminSection = "users" | "projects" | "quotas" | "reserved";
export const ADMIN_SECTIONS: AdminSection[] = [
  "users",
  "projects",
  "quotas",
  "reserved",
];

/** Map a section id to its i18n key under `admin`. */
const SECTION_KEY: Record<AdminSection, string> = {
  users: "users",
  projects: "projects",
  quotas: "quotas",
  reserved: "reservedWords",
};

export interface AdminLayoutProps {
  children: React.ReactNode;
  section: AdminSection;
  onSectionChange: (section: AdminSection) => void;
  className?: string;
}

export function AdminLayout({
  children,
  section,
  onSectionChange,
  className,
}: AdminLayoutProps) {
  const t = useTranslations("admin");
  const tNav = useTranslations("nav");

  const breadcrumbs = [
    { label: tNav("dashboard"), href: "/dashboard" },
    { label: t("title") },
  ];

  return (
    <DashboardLayout breadcrumbs={breadcrumbs}>
      <div className={cn("space-y-6", className)}>
        <SectionHeading level={1} title={t("title")} />

        {/* Desktop / tablet: secondary tab bar. */}
        <div className="hidden sm:block">
          <Tabs
            value={section}
            onValueChange={(v) => v != null && onSectionChange(v as AdminSection)}
          >
            <TabsList variant="line">
              {ADMIN_SECTIONS.map((s) => (
                <TabsTrigger key={s} value={s}>
                  {t(SECTION_KEY[s])}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
        </div>

        {/* Phone: a Select (the four labels would crowd a 375px bar). */}
        <div className="sm:hidden">
          <Select
            value={section}
            onValueChange={(v) => v != null && onSectionChange(v as AdminSection)}
          >
            <SelectTrigger className="w-full" aria-label={t("title")}>
              <SelectValue>
                {(v: AdminSection) => t(SECTION_KEY[v])}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              {ADMIN_SECTIONS.map((s) => (
                <SelectItem key={s} value={s}>
                  {t(SECTION_KEY[s])}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>

        {/* The active admin panel. */}
        <div>{children}</div>
      </div>
    </DashboardLayout>
  );
}
