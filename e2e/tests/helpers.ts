import { execFileSync } from "node:child_process";
import { mkdtempSync, writeFileSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { BrowserContext, Page } from "@playwright/test";
import { expect } from "@playwright/test";

/**
 * Shared E2E helpers + the test-environment contract.
 *
 * Everything that couples the suite to "how the stack was composed" lives here as
 * a single env-driven config, so the same specs run against a local box and the
 * CI-composed stack. The Prove/CI runner sets these to match the edge overlay it
 * brings up (see the workflow's e2e job and the suite README).
 */

/** The single-origin base domain the edge overlay serves (KOTOJI_BASE_DOMAIN). */
export const BASE_DOMAIN = process.env.KOTOJI_E2E_BASE_DOMAIN ?? "hosting.localhost";

/** The control-host origin (scheme + bare base domain), e.g. http://hosting.localhost. */
export const BASE_URL = process.env.KOTOJI_E2E_BASE_URL ?? `http://${BASE_DOMAIN}`;

/** http | https — derived from BASE_URL so served-subdomain URLs match the scheme. */
export const SCHEME = new URL(BASE_URL).protocol.replace(":", "");

/**
 * The host port of the single origin, derived from BASE_URL. Empty for the
 * default port (80/443). When the edge is published on a non-default host port
 * (e.g. http://hosting.localhost:18080 on a box where 80 is already taken),
 * served-subdomain URLs must carry that SAME port — the subdomain shares the one
 * origin. Kept as ":<port>" (or "") so it slots straight into a URL.
 */
export const PORT_SUFFIX = new URL(BASE_URL).port ? `:${new URL(BASE_URL).port}` : "";

/**
 * The admin password the test sets during first-run setup. The stack is started
 * WITHOUT KOTOJI_AUTH_ADMIN_PASSWORD so the first-run setup screen appears; the
 * test chooses this password (so we exercise the real first-run journey), then
 * signs in with it. Overridable for a stack that was pre-seeded with a password.
 */
export const ADMIN_PASSWORD =
  process.env.KOTOJI_E2E_ADMIN_PASSWORD ?? "e2e-admin-pass-123";

/** Build the served URL for a site handle on its subdomain (single origin). */
export function siteUrl(handle: string): string {
  // Carry the single-origin host port (PORT_SUFFIX) so a subdomain on a non-80
  // edge (e.g. :18080) is reachable; empty for the default port.
  return `${SCHEME}://${handle}.${BASE_DOMAIN}${PORT_SUFFIX}`;
}

/**
 * A collision-resistant, DNS-label-safe site handle for one test run. Lowercase
 * letters/digits/hyphens only (CANONICAL §5.1), starts with a letter, well under
 * the 63-char cap. Uniqueness avoids "handle taken" across reruns on a stack
 * whose Postgres volume persists.
 */
export function uniqueHandle(prefix = "e2e"): string {
  const rand = Math.random().toString(36).slice(2, 8);
  const stamp = Date.now().toString(36).slice(-5);
  return `${prefix}-${stamp}${rand}`.toLowerCase();
}

/**
 * Pin the UI locale to English BEFORE any navigation so visible-text selectors
 * are deterministic. The app reads the NEXT_LOCALE cookie (i18n/config.ts); we
 * set it host-wide for the control origin. Combined with the config's
 * Accept-Language header this is belt-and-suspenders against a runner default.
 */
export async function pinEnglishLocale(context: BrowserContext): Promise<void> {
  await context.addCookies([
    {
      name: "NEXT_LOCALE",
      value: "en",
      domain: new URL(BASE_URL).hostname,
      path: "/",
    },
  ]);
}

/**
 * Build a minimal seed .zip on disk containing a single index.html carrying a
 * unique marker, and return { path, marker }. The create-site "From a zip" flow
 * uploads this onto the new site's draft branch — a deterministic, Monaco-free
 * way to put known content into the editor's file tree.
 *
 * We shell out to the system `zip` (present on ubuntu-latest and dev boxes)
 * rather than add a Node zip dependency, keeping the suite dependency-light.
 */
export function makeSeedZip(marker: string): { path: string; marker: string } {
  const dir = mkdtempSync(join(tmpdir(), "kotoji-e2e-"));
  const srcDir = join(dir, "site");
  mkdirSync(srcDir, { recursive: true });
  const html = [
    "<!doctype html>",
    '<html lang="en">',
    "<head><meta charset=\"utf-8\"><title>kotoji e2e</title></head>",
    `<body><h1>${marker}</h1></body>`,
    "</html>",
    "",
  ].join("\n");
  writeFileSync(join(srcDir, "index.html"), html, "utf8");
  const zipPath = join(dir, "seed.zip");
  // -j junks paths so index.html sits at the archive root (the served doc root).
  execFileSync("zip", ["-j", "-q", zipPath, join(srcDir, "index.html")]);
  return { path: zipPath, marker };
}

/**
 * Drive the first-run admin setup OR the password sign-in, leaving the browser
 * authenticated on /dashboard. Idempotent against instance state:
 *  - fresh instance  -> the SetupForm renders; we set + confirm the password,
 *  - already set up   -> the sign-in form renders; we enter the same password.
 * Either way we end authenticated. This makes the suite re-runnable against a
 * stack whose DB volume persisted a prior run's admin credential.
 */
export async function loginAsAdmin(page: Page): Promise<void> {
  await page.goto("/login");

  // Distinguish first-run (setup) from returning (sign-in) by which control is
  // present. Both share the (auth) card chrome; we wait for either to appear.
  const setupHeading = page.getByRole("heading", {
    name: "Set the admin password",
  });
  const signInHeading = page.getByRole("heading", { name: "Sign in to kotoji" });

  await expect(setupHeading.or(signInHeading)).toBeVisible();

  if (await setupHeading.isVisible()) {
    // First-run: choose the admin password. The two password fields are the only
    // password inputs on the setup form; target them by accessible label.
    // Required fields render their label as "New password * (Required)" (an
    // asterisk + a visually-hidden "(Required)" marker is part of the accessible
    // name), so an exact "New password" match misses. Anchor on the label PREFIX
    // with a regex — still unambiguous within the form, robust to the marker.
    await page.getByLabel(/^New password/).fill(ADMIN_PASSWORD);
    await page.getByLabel(/^Confirm password/).fill(ADMIN_PASSWORD);
    await page.getByRole("button", { name: "Set password" }).click();
  } else {
    // Returning: the break-glass admin-password form. The submit button label is
    // the same "Sign in to kotoji" string; scope the click to the submit button.
    await page.getByLabel(/^Admin password/).fill(ADMIN_PASSWORD);
    await page.getByRole("button", { name: "Sign in to kotoji" }).click();
  }

  // Both flows land authenticated on /dashboard (setup navigates client-side to
  // `next`; password login 302s there). Assert on the dashboard heading.
  await page.waitForURL(/\/dashboard(\?.*)?$/, { timeout: 30_000 });
  await expect(
    page.getByRole("heading", { name: "Dashboard", level: 1 })
  ).toBeVisible();
}
