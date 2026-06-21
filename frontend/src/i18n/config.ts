/**
 * i18n configuration (CANONICAL.md decision #5: next-intl, ja default + en).
 *
 * Routing strategy: COOKIE-based, NO URL locale prefix. The design's routes are
 * flat (`/dashboard`, `/sites/[handle]`, …) and must stay deep-linkable without
 * a `/ja` or `/en` segment, so we keep a single route tree and select the locale
 * from the `NEXT_LOCALE` cookie (falling back to ja). The proxy persists the
 * cookie; a LocaleSwitcher (organism layer) sets it.
 */

export const locales = ["ja", "en"] as const;
export type Locale = (typeof locales)[number];

/** Default language at launch is Japanese (CANONICAL.md decision #5). */
export const defaultLocale: Locale = "ja";

/** Cookie the active locale is stored in (read by the request config + proxy). */
export const LOCALE_COOKIE = "NEXT_LOCALE";

/** Human labels for the LocaleSwitcher. */
export const localeLabels: Record<Locale, string> = {
  ja: "日本語",
  en: "English",
};

/** Narrow an arbitrary string to a supported Locale, defaulting safely. */
export function isLocale(value: string | undefined | null): value is Locale {
  return value != null && (locales as readonly string[]).includes(value);
}

/** Resolve any candidate to a valid locale (used by the request config). */
export function resolveLocale(candidate: string | undefined | null): Locale {
  return isLocale(candidate) ? candidate : defaultLocale;
}
