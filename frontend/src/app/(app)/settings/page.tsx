"use client";

/**
 * Instance / account Settings page (/settings).
 *
 * This is the INSTANCE-level settings surface — distinct from the per-project
 * /sites/{handle}/settings page. It is reachable from the avatar menu ("設定")
 * and the sidebar bottom, and is visible to any AUTHENTICATED user (the (app)
 * group layout's AuthGate guards the route).
 *
 * Sections:
 *  (A) GitHub連携 — ADMIN ONLY (rendered only when me.user.isAdmin). The instance
 *      GitHub mirror config (enable / org / write-only PAT / write-only webhook
 *      secret), persisted via PUT /api/admin/github.
 *  (B) MCP 接続ガイド — shown to EVERYONE. A read-only tutorial for pointing an AI
 *      client at this instance's /mcp endpoint with a per-project token.
 *
 * Uses DashboardLayout for the standard authenticated chrome (sidebar + topnav).
 */

import { useTranslations } from "next-intl";

import { DashboardLayout } from "@/components/templates";
import { SectionHeading } from "@/components/atoms";
import {
  GitHubAdminSection,
  McpGuideSection,
} from "@/components/organisms";
import { useMe } from "@/lib/api/hooks";

export default function SettingsPage() {
  const t = useTranslations("settings");
  const { data: me } = useMe();

  // Admin-only gate for the GitHub config section (CANONICAL §6 is_admin). The
  // MCP guide below is shown to everyone.
  const isAdmin = me?.user.isAdmin ?? false;

  return (
    <DashboardLayout>
      <div className="mx-auto w-full max-w-3xl space-y-8">
        <SectionHeading
          level={1}
          title={t("instanceTitle")}
          description={t("instanceSubtitle")}
        />

        {/* (A) Instance GitHub mirror config — admin only. */}
        {isAdmin ? <GitHubAdminSection /> : null}

        {/* (B) MCP connection guide — everyone. */}
        <McpGuideSection />
      </div>
    </DashboardLayout>
  );
}
