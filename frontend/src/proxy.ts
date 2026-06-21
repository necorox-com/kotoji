/**
 * Next 16 Proxy (the renamed `middleware`). Runs before routes render.
 *
 * Sole job here: ensure a `NEXT_LOCALE` cookie exists so next-intl's
 * cookie-based locale selection (src/i18n/request.ts) is stable. First-time
 * visitors are negotiated from Accept-Language (en if they clearly prefer
 * English, else ja per CANONICAL.md decision #5 ja-default). Once set, the
 * cookie is authoritative — a LocaleSwitcher can overwrite it.
 *
 * Auth route-guarding is intentionally NOT done here: the design guards in the
 * (app) layout via /api/me (design.md §4.3), and the Proxy must not block on a
 * cross-service API call. The matcher excludes static assets and API routes.
 */

import { NextResponse, type NextRequest } from "next/server";
import { defaultLocale, isLocale, LOCALE_COOKIE, locales } from "./i18n/config";

/** Pick the best locale from Accept-Language, biased to ja (the default). */
function negotiateLocale(header: string | null): string {
  if (!header) return defaultLocale;
  // Parse "en-US,en;q=0.9,ja;q=0.8" into ordered base languages.
  const ordered = header
    .split(",")
    .map((part) => {
      const [tag, q] = part.trim().split(";q=");
      return { tag: tag.toLowerCase().split("-")[0], q: q ? Number(q) : 1 };
    })
    .sort((a, b) => b.q - a.q);

  for (const { tag } of ordered) {
    if (locales.includes(tag as (typeof locales)[number])) return tag;
  }
  return defaultLocale;
}

export function proxy(request: NextRequest) {
  const existing = request.cookies.get(LOCALE_COOKIE)?.value;
  // Already have a valid locale cookie: nothing to do.
  if (isLocale(existing)) {
    return NextResponse.next();
  }

  const locale = negotiateLocale(request.headers.get("accept-language"));
  const response = NextResponse.next();
  response.cookies.set(LOCALE_COOKIE, locale, {
    path: "/",
    maxAge: 60 * 60 * 24 * 365, // 1 year
    sameSite: "lax",
  });
  return response;
}

export const config = {
  // Run on app pages only; skip Next internals, API, and static asset files so
  // the cookie write never interferes with CSS/JS/image delivery.
  matcher: ["/((?!_next/static|_next/image|favicon.ico|api/).*)"],
};
