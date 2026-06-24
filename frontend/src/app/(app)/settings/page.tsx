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
 *  (A2) ドメイン / URL — ADMIN ONLY. The instance base domain + control base URL
 *      (WordPress-style precedence: env OVERRIDES DB OVERRIDES request-derived),
 *      persisted via PUT /api/admin/domain. Env-locked fields are read-only.
 *  (A3) サインイン / 認証 — ADMIN ONLY. The instance Google OIDC config (enable /
 *      issuer / client id / write-only client secret / derived redirect URL /
 *      fail-closed allowlists), persisted via PUT /api/admin/oidc with the same
 *      env-OVERRIDES-DB precedence. Enabling it adds the Google button to /login.
 *  (A4) TLS / HTTPS — ADMIN ONLY. A read-only informational panel for kotoji-
 *      native on-demand TLS (the third deploy mode, docs §4.5): how auto-TLS
 *      works and which host it would cover (derived from controlBaseURL). The
 *      mode is a DEPLOY choice (docker-compose.tls.yml binds :80/:443); the
 *      backend exposes no live TLS read endpoint, so the panel is static/honest.
 *  (B) MCP / API トークン — shown to EVERYONE. The user's OWN per-user tokens
 *      (CANONICAL §6, re-architected model): a token is owned by the user and
 *      automatically covers every project they're a member of. Issue (show-once
 *      plaintext) / list / revoke via /api/tokens.
 *  (C) MCP 接続ガイド — shown to EVERYONE. A read-only tutorial for pointing an AI
 *      client at this instance's /mcp endpoint with one of the above tokens.
 *
 * Uses DashboardLayout for the standard authenticated chrome (sidebar + topnav).
 */

import { useTranslations } from "next-intl";

import { DashboardLayout } from "@/components/templates";
import { SectionHeading } from "@/components/atoms";
import {
  GitHubAdminSection,
  DomainAdminSection,
  OIDCAdminSection,
  TlsSection,
  AccountTokenPanel,
  McpGuideSection,
} from "@/components/organisms";
import { useMe } from "@/lib/api/hooks";

export default function SettingsPage() {
  const t = useTranslations("settings");
  const { data: me } = useMe();

  // Admin-only gate for the GitHub config section (CANONICAL §6 is_admin). The
  // token panel + MCP guide below are shown to everyone.
  const isAdmin = me?.user.isAdmin ?? false;
  // Whether the user may create sites — gates the per-token "may create sites"
  // toggle (the server caps any requested capability to the user's own).
  const canCreateSites = me?.user.canCreateSites ?? false;

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

        {/* (A2) Instance domain / URL config — admin only (env > DB > derived). */}
        {isAdmin ? <DomainAdminSection /> : null}

        {/* (A3) Sign-in / 認証 — admin only. Google OIDC config (enable / issuer /
            client id / write-only secret / derived redirect / fail-closed
            allowlists), persisted via PUT /api/admin/oidc. Enabling it adds the
            Google button to the login page (authProviders). */}
        {isAdmin ? <OIDCAdminSection /> : null}

        {/* (A4) TLS / HTTPS — admin only. Read-only informational panel for
            kotoji-native on-demand TLS (the third deploy mode, docs §4.5): how
            auto-TLS works + which host it would cover (from controlBaseURL).
            Enabling auto-TLS is a DEPLOY choice (docker-compose.tls.yml binds
            :80/:443); the backend exposes no live TLS read endpoint yet, so this
            panel does not claim the running mode. */}
        {isAdmin ? <TlsSection /> : null}

        {/* (B) Per-user MCP/API tokens — everyone (the user's own tokens). */}
        <AccountTokenPanel canCreateSites={canCreateSites} />

        {/* (C) MCP connection guide — everyone. */}
        <McpGuideSection />
      </div>
    </DashboardLayout>
  );
}
