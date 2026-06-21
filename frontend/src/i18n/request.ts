/**
 * next-intl request configuration (called per request on the server).
 *
 * Cookie-based locale selection (no URL prefix): read NEXT_LOCALE, fall back to
 * the default (ja). Messages are loaded from src/messages/{locale}.json. This
 * file is referenced by next.config.ts via the next-intl plugin.
 */

import { cookies } from "next/headers";
import { getRequestConfig } from "next-intl/server";
import { LOCALE_COOKIE, resolveLocale } from "./config";

export default getRequestConfig(async () => {
  // Read the persisted locale cookie; resolveLocale guards bad/missing values.
  const store = await cookies();
  const locale = resolveLocale(store.get(LOCALE_COOKIE)?.value);

  return {
    locale,
    // Static import map keeps messages tree-shakeable and type-checkable.
    messages: (await import(`../messages/${locale}.json`)).default,
  };
});
