"use client";

/**
 * Admin page (design.md §3.5 Admin).
 *
 * Admin-guarded (AuthGate requireAdmin → non-admins bounce to /dashboard). Uses
 * AdminLayout for the chrome + the secondary sub-nav (Users / Projects / Quotas /
 * Reserved words), with the active section held in local state and reflected in a
 * `?section=` search param so it is shareable/refresh-stable.
 *
 * The admin REST surface is not yet in the OpenAPI contract (docs/contracts/
 * openapi.yaml has no /api/admin routes; design.md §5 gap #2/#11), so each panel
 * renders an honest "coming soon" empty state rather than a fake table — never a
 * fake success (design.md principle #4). The structure (sections, headings,
 * descriptions) is in place so wiring real endpoints later is additive.
 */

import { useCallback, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { useTranslations } from "next-intl";
import { Construction } from "lucide-react";
import { AuthGate, AdminLayout, ADMIN_SECTIONS } from "@/components/templates";
import type { AdminSection } from "@/components/templates";
import { EmptyState } from "@/components/molecules/empty-state";

/** Map a section to its description i18n key under `admin`. */
const SECTION_DESC: Record<AdminSection, string> = {
  users: "usersDescription",
  projects: "projectsDescription",
  quotas: "quotasDescription",
  reserved: "reservedWordsDescription",
};

/** Narrow a raw query string to a valid section, defaulting to "users". */
function toSection(raw: string | null): AdminSection {
  return ADMIN_SECTIONS.includes(raw as AdminSection)
    ? (raw as AdminSection)
    : "users";
}

function AdminContent() {
  const t = useTranslations("admin");
  const router = useRouter();
  const searchParams = useSearchParams();

  const [section, setSection] = useState<AdminSection>(() =>
    toSection(searchParams.get("section"))
  );

  // Keep the URL in sync so the active section survives refresh/share.
  const onSectionChange = useCallback(
    (next: AdminSection) => {
      setSection(next);
      const params = new URLSearchParams(searchParams.toString());
      params.set("section", next);
      router.replace(`/admin?${params.toString()}`);
    },
    [router, searchParams]
  );

  return (
    <AdminLayout section={section} onSectionChange={onSectionChange}>
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">{t(SECTION_DESC[section])}</p>
        <EmptyState
          icon={Construction}
          title={t("comingSoon.title")}
          body={t("comingSoon.body")}
        />
      </div>
    </AdminLayout>
  );
}

export default function AdminPage() {
  // Admin routes additionally require instance-admin (design.md §4.3).
  return (
    <AuthGate requireAdmin>
      <AdminContent />
    </AuthGate>
  );
}
