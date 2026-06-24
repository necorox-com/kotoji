"use client";

/**
 * TlsSection (organism) — the INSTANCE-level TLS / HTTPS status panel on the
 * /settings page (sibling of GitHubAdminSection / DomainAdminSection). ADMIN-ONLY:
 * the page mounts it solely when `me.user.isAdmin`.
 *
 * HONESTY / SCOPE (read this before "improving" the panel):
 *  - kotoji has THREE deploy modes for TLS (docs/architecture.md §4.5):
 *      (1) off (DEFAULT) — kotoji speaks plain HTTP on its existing ports and TLS
 *          is terminated by YOUR edge (a reverse proxy / the Traefik overlay /
 *          Cloudflare). This is what the live, Cloudflare-fronted deployment uses.
 *      (2) auto — kotoji TERMINATES TLS ITSELF via CertMagic on-demand: a single
 *          :443 listener fronts both planes and :80 redirects to HTTPS (+ solves
 *          the ACME HTTP-01 challenge). Certificates are issued per-host on the
 *          fly (Let's Encrypt, TLS-ALPN-01 / HTTP-01) the first time a handshake
 *          arrives for a host this instance can actually serve (the control host
 *          or an existing site/preview host); unknown hosts are refused. No
 *          wildcard, no DNS-01 token, no ACME secret in env.
 *  - WHICH mode is active is a DEPLOY choice: enabling auto-TLS means composing
 *    `deploy/docker-compose.tls.yml` (publishes :80 + :443, sets
 *    KOTOJI_TLS_MODE=auto). It CANNOT be flipped from this UI — the listeners are
 *    bound at process start. Within auto mode, issuance/renewal is then FULLY
 *    AUTOMATIC from DNS; there is nothing to click.
 *  - The backend currently exposes NO read endpoint for the live TLS state
 *    (no GET /api/admin/tls, no `tls*` fields on GET /api/config). Adding one is
 *    explicitly out of scope for this task. So this panel is a READ-ONLY,
 *    informational summary of how kotoji-native TLS works + which hosts it would
 *    cover — derived from the one thing the API does expose: the effective
 *    control base URL (GET /api/config → controlBaseURL). It does NOT claim to
 *    report the running mode or the set of issued certs (we don't have that data
 *    yet). See the StructuredOutput "gaps" note.
 *
 * If/when the backend grows a GET /api/admin/tls (mode, ca, email, issued-host
 * list), swap the static copy below for that read and show the live mode +
 * per-host cert list. The i18n keys (settings.tls.*) are already factored so the
 * dynamic version can reuse them.
 */

import { useTranslations } from "next-intl";
import { ShieldCheck, Server, Cloud, FileLock2 } from "lucide-react";

import { CodeText } from "@/components/atoms/code-text";
import { Link } from "@/components/atoms/link";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useConfig } from "@/lib/api/hooks";

export interface TlsSectionProps {
  className?: string;
}

// Docs anchor for the third deploy mode (auto-TLS) — kept here so the copy and
// the link stay in one place if the heading moves.
const ARCHITECTURE_TLS_DOCS =
  "https://github.com/necorox-com/kotoji/blob/main/docs/architecture.md#45-kotoji-native-on-demand-tls-third-deploy-mode--kotoji_tls_mode_auto";

export function TlsSection({ className }: TlsSectionProps) {
  const t = useTranslations("settings");

  // The effective control base URL is the only TLS-relevant fact the API exposes
  // today (GET /api/config). We surface its HOST as the canonical host that
  // auto-TLS would obtain a certificate for first — concrete + honest, without
  // pretending to know the running mode. Falls back gracefully while loading /
  // on a malformed value.
  const { data: config } = useConfig();
  const controlHost = hostOf(config?.controlBaseURL);

  return (
    <Card className={className} aria-labelledby="settings-tls">
      <CardHeader>
        <CardTitle
          id="settings-tls"
          className="flex items-center gap-2 text-lg"
        >
          <ShieldCheck className="size-5 shrink-0" aria-hidden="true" />
          {t("tlsTitle")}
        </CardTitle>
        <CardDescription>{t("tlsDescription")}</CardDescription>
      </CardHeader>

      <CardContent className="space-y-6">
        {/* (1) off — TLS handled by your edge (the live, default mode). */}
        <section className="space-y-2" aria-labelledby="tls-mode-off">
          <h3
            id="tls-mode-off"
            className="flex items-center gap-1.5 text-sm font-semibold text-foreground"
          >
            <Cloud className="size-4 shrink-0" aria-hidden="true" />
            {t("tlsModeOffTitle")}
          </h3>
          <p className="text-sm text-muted-foreground">{t("tlsModeOffBody")}</p>
        </section>

        {/* (2) auto — kotoji-managed automatic HTTPS via CertMagic on-demand. */}
        <section className="space-y-2" aria-labelledby="tls-mode-auto">
          <h3
            id="tls-mode-auto"
            className="flex items-center gap-1.5 text-sm font-semibold text-foreground"
          >
            <Server className="size-4 shrink-0" aria-hidden="true" />
            {t("tlsModeAutoTitle")}
          </h3>
          <p className="text-sm text-muted-foreground">{t("tlsModeAutoBody")}</p>
          {/* Honest enablement note: this is a DEPLOY choice (the overlay binds
              :80/:443); within auto mode issuance is automatic from DNS. */}
          <p className="text-sm text-muted-foreground">
            {t.rich("tlsModeAutoEnable", {
              overlay: () => <CodeText>deploy/docker-compose.tls.yml</CodeText>,
              env: () => <CodeText>KOTOJI_TLS_MODE=auto</CodeText>,
            })}
          </p>
        </section>

        {/* (3) What auto-TLS would cover — the effective control host (the one
            TLS fact the API exposes). Sites/preview hosts are also covered on
            demand; we name the control host concretely. */}
        <section className="space-y-2" aria-labelledby="tls-coverage">
          <h3
            id="tls-coverage"
            className="flex items-center gap-1.5 text-sm font-semibold text-foreground"
          >
            <FileLock2 className="size-4 shrink-0" aria-hidden="true" />
            {t("tlsCoverageTitle")}
          </h3>
          <p className="text-sm text-muted-foreground">
            {t("tlsCoverageBody")}
          </p>
          <dl className="text-sm">
            <div className="flex flex-wrap items-center gap-2">
              <dt className="text-muted-foreground">{t("tlsControlHost")}</dt>
              <dd>
                {controlHost ? (
                  <CodeText>{controlHost}</CodeText>
                ) : (
                  <span className="text-muted-foreground">{t("tlsHostUnknown")}</span>
                )}
              </dd>
            </div>
          </dl>
        </section>

        {/* Docs link — the third deploy mode is documented in architecture.md. */}
        <p className="text-sm text-muted-foreground">
          {t.rich("tlsDocsHint", {
            link: (chunks) => (
              <Link href={ARCHITECTURE_TLS_DOCS} variant="inline" external>
                {chunks}
              </Link>
            ),
          })}
        </p>
      </CardContent>
    </Card>
  );
}

/**
 * hostOf — extract the host (host:port) from an absolute URL string, or undefined
 * when the value is absent / unparseable. Used to turn the effective control base
 * URL into the host that auto-TLS would obtain a certificate for. Resilient: never
 * throws (a bad value just yields undefined → the "unknown" placeholder).
 */
function hostOf(url: string | undefined): string | undefined {
  if (!url) return undefined;
  try {
    return new URL(url).host || undefined;
  } catch {
    return undefined;
  }
}
