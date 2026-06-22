"use client";

/**
 * McpGuideSection (organism) — the "MCP 接続ガイド / Connect via MCP" tutorial on
 * the /settings page. Shown to EVERYONE (any authenticated user), not just
 * admins. It is read-only documentation: how to point an AI client (Claude Code
 * / Claude Desktop) at this instance's MCP endpoint using a per-project token.
 *
 * The MCP endpoint URL is rendered DYNAMICALLY from the live origin
 * (`${location.origin}/mcp`) so the copy snippets are correct on any deployment
 * without baked-in config. We read the origin only AFTER mount (via the
 * useMounted idiom — same as UserMenu) to avoid an SSR/hydration mismatch; a
 * neutral placeholder shows during the server render.
 *
 * Per-project tokens are minted on a project's Settings → MCP tokens (the
 * McpTokenPanel); this guide links there rather than duplicating that flow.
 */

import { useSyncExternalStore } from "react";
import { Plug, Terminal, Wrench, ShieldAlert } from "lucide-react";
import { useTranslations } from "next-intl";

import { CopyableUrl } from "@/components/molecules/copyable-url";
import { Link } from "@/components/atoms/link";
import { CodeText } from "@/components/atoms/code-text";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

// Placeholder origin used during SSR / before mount so the snippets render
// stable markup; replaced with the real origin client-side.
const ORIGIN_PLACEHOLDER = "https://kotoji.example.com";

// The MCP tools a connected client can call (mcp.md surface). Listed verbatim so
// users know exactly what their AI assistant can do once connected.
const MCP_TOOLS = [
  "list_sites",
  "list_files",
  "read_file",
  "write_file",
  "save",
  "publish",
  "get_diff",
  "get_log",
  "rollback",
] as const;

/** useMounted — true only after client mount (codebase idiom; UserMenu §4.4). */
function useMounted(): boolean {
  return useSyncExternalStore(
    () => () => {},
    () => true,
    () => false,
  );
}

export interface McpGuideSectionProps {
  className?: string;
}

export function McpGuideSection({ className }: McpGuideSectionProps) {
  const t = useTranslations("settings");

  // Read the live origin only after mount (hydration-safe). Falls back to a
  // neutral placeholder during SSR.
  const mounted = useMounted();
  const origin =
    mounted && typeof window !== "undefined"
      ? window.location.origin
      : ORIGIN_PLACEHOLDER;

  const endpoint = `${origin}/mcp`;

  // Example Claude Code CLI command (HTTP transport + bearer token header).
  const cliCommand = `claude mcp add --transport http kotoji ${endpoint} --header "Authorization: Bearer kotoji_pat_..."`;

  // Example Claude Desktop config block (mcpServers entry with url + auth header).
  const desktopConfig = JSON.stringify(
    {
      mcpServers: {
        kotoji: {
          url: endpoint,
          headers: {
            Authorization: "Bearer kotoji_pat_...",
          },
        },
      },
    },
    null,
    2,
  );

  return (
    <Card className={className} aria-labelledby="settings-mcp-guide">
      <CardHeader>
        <CardTitle
          id="settings-mcp-guide"
          className="flex items-center gap-2 text-lg"
        >
          <Plug className="size-5 shrink-0" aria-hidden="true" />
          {t("mcpGuide")}
        </CardTitle>
        <CardDescription>{t("mcpGuideDescription")}</CardDescription>
      </CardHeader>

      <CardContent className="space-y-6">
        {/* 1. Endpoint URL — rendered dynamically + copyable. */}
        <section className="space-y-2" aria-labelledby="mcp-guide-endpoint">
          <h3
            id="mcp-guide-endpoint"
            className="text-sm font-semibold text-foreground"
          >
            {t("mcpEndpointTitle")}
          </h3>
          <p className="text-sm text-muted-foreground">
            {t("mcpEndpointBody")}
          </p>
          <CopyableUrl value={endpoint} className="max-w-md" />
        </section>

        {/* 2. Per-project token — link to the project Settings → MCP tokens. */}
        <section className="space-y-2" aria-labelledby="mcp-guide-token">
          <h3
            id="mcp-guide-token"
            className="text-sm font-semibold text-foreground"
          >
            {t("mcpTokenTitle")}
          </h3>
          <p className="text-sm text-muted-foreground">
            {t.rich("mcpTokenBody", {
              link: (chunks) => (
                <Link href="/dashboard" variant="inline">
                  {chunks}
                </Link>
              ),
            })}
          </p>
        </section>

        {/* 3. Claude Code CLI command. */}
        <section className="space-y-2" aria-labelledby="mcp-guide-cli">
          <h3
            id="mcp-guide-cli"
            className="flex items-center gap-1.5 text-sm font-semibold text-foreground"
          >
            <Terminal className="size-4 shrink-0" aria-hidden="true" />
            {t("mcpCliTitle")}
          </h3>
          <p className="text-sm text-muted-foreground">{t("mcpCliBody")}</p>
          <pre className="max-h-60 overflow-auto rounded-lg border border-border bg-muted p-3 font-mono text-xs leading-relaxed text-foreground">
            <code>{cliCommand}</code>
          </pre>
          <CopyableUrl
            value={cliCommand}
            label={t("mcpCopyCommand")}
            className="justify-end"
          />
        </section>

        {/* 4. Claude Desktop config JSON. */}
        <section className="space-y-2" aria-labelledby="mcp-guide-desktop">
          <h3
            id="mcp-guide-desktop"
            className="text-sm font-semibold text-foreground"
          >
            {t("mcpDesktopTitle")}
          </h3>
          <p className="text-sm text-muted-foreground">
            {t("mcpDesktopBody")}
          </p>
          <pre className="max-h-72 overflow-auto rounded-lg border border-border bg-muted p-3 font-mono text-xs leading-relaxed text-foreground">
            <code>{desktopConfig}</code>
          </pre>
          <CopyableUrl
            value={desktopConfig}
            label={t("mcpCopyConfig")}
            className="justify-end"
          />
        </section>

        {/* 5. Available tools. */}
        <section className="space-y-2" aria-labelledby="mcp-guide-tools">
          <h3
            id="mcp-guide-tools"
            className="flex items-center gap-1.5 text-sm font-semibold text-foreground"
          >
            <Wrench className="size-4 shrink-0" aria-hidden="true" />
            {t("mcpToolsTitle")}
          </h3>
          <p className="text-sm text-muted-foreground">{t("mcpToolsBody")}</p>
          <ul className="flex flex-wrap gap-1.5">
            {MCP_TOOLS.map((tool) => (
              <li key={tool}>
                <CodeText>{tool}</CodeText>
              </li>
            ))}
          </ul>
        </section>

        {/* 6. Cloudflare Bot Fight Mode note. */}
        <Alert>
          <ShieldAlert aria-hidden="true" />
          <AlertTitle>{t("mcpCloudflareTitle")}</AlertTitle>
          <AlertDescription>{t("mcpCloudflareBody")}</AlertDescription>
        </Alert>
      </CardContent>
    </Card>
  );
}
