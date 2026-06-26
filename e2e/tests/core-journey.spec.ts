import { test, expect } from "@playwright/test";
import {
  ADMIN_PASSWORD,
  BASE_DOMAIN,
  loginAsAdmin,
  makeSeedZip,
  pinEnglishLocale,
  siteUrl,
  uniqueHandle,
} from "./helpers";

/**
 * kotoji CORE USER JOURNEY (password auth, single-origin edge in HTTP mode).
 *
 * One ordered, serial flow proving the load-bearing path a real operator walks:
 *
 *   1. first-run admin setup / password sign-in  -> authenticated dashboard
 *   2. create a site (seeded From a zip)          -> ProjectDetail, file in tree
 *   3. publish the draft                           -> published state
 *   4. the published content is served on its subdomain (<handle>.<base domain>)
 *   5. issue a per-user MCP/API token on /settings -> show-once secret + scopes
 *
 * Selectors prefer roles + accessible names + the app's existing aria-labels, so
 * they survive styling churn. Steps run in declared order (serial) because each
 * depends on the previous one's mutation of the same instance.
 */

// Serial: the steps form one journey on one shared instance; a failure early
// should not leave later steps running against a half-built site.
test.describe.configure({ mode: "serial" });

test.describe("core journey @ single-origin edge (HTTP)", () => {
  // One site handle + one content marker for the whole journey.
  const handle = uniqueHandle();
  const { path: zipPath, marker } = makeSeedZip(
    `kotoji-e2e-marker-${handle}`
  );

  test.beforeEach(async ({ context }) => {
    // Pin English BEFORE the first navigation so visible-text selectors are
    // deterministic regardless of the runner's locale.
    await pinEnglishLocale(context);
  });

  test("1. first-run admin setup / sign-in lands on the dashboard", async ({
    page,
  }) => {
    await loginAsAdmin(page);
    // Sanity: the New site CTA exists for an authenticated admin.
    await expect(
      page.getByRole("link", { name: "New site" }).first()
    ).toBeVisible();
  });

  test("2. create a site seeded from a zip; the file lands in the tree", async ({
    page,
  }) => {
    await loginAsAdmin(page);

    await page.goto("/sites/new");
    await expect(
      page.getByRole("heading", { name: "Create a new site", level: 1 })
    ).toBeVisible();

    // ① Start mode: From a zip. The mode cards are accessible radios (the visible
    // label text is the accessible name on the native radio inside the styled
    // <label>).
    // The radio is a visually-hidden <input> inside a styled label card; a
    // decorative SVG icon in the card intercepts pointer events on the input, so
    // a normal .check() (which clicks the input) is blocked. force:true checks the
    // associated radio without the obscured click — the value still binds.
    await page.getByRole("radio", { name: "From a zip" }).check({ force: true });

    // ② Handle. Live-validated; we use a unique, valid handle so no 409. The
    // label renders as "Handle * (Required)" (required marker in the accessible
    // name), so anchor on the prefix rather than an exact match.
    await page.getByLabel(/^Handle/).fill(handle);

    // ③ The zip file picker is an sr-only <input type=file> behind a styled
    // label; set its files directly by the input id.
    await page.locator("#create-zip").setInputFiles(zipPath);

    // Submit. The button is disabled until the form is valid AND (zip mode) a
    // file is chosen; expect.toBeEnabled gates on that before clicking.
    const createBtn = page.getByRole("button", { name: "Create", exact: true });
    await expect(createBtn).toBeEnabled();
    await createBtn.click();

    // The page redirects to ProjectDetail (the Files/editor section) after the
    // seed zip is uploaded onto the draft branch.
    await page.waitForURL(new RegExp(`/sites/${handle}(\\?.*)?$`), {
      timeout: 30_000,
    });

    // The seeded index.html appears in the FileTree (role=tree). Proves the
    // create + zip-upload onto the draft branch both succeeded.
    const tree = page.getByRole("tree", { name: "Files" });
    await expect(tree).toBeVisible();
    await expect(tree.getByText("index.html", { exact: true })).toBeVisible();
  });

  test("3. publish the draft to the published branch", async ({ page }) => {
    await loginAsAdmin(page);

    // Navigate straight to the Publish section by URL (robust against the
    // responsive tab/menu layout).
    await page.goto(`/sites/${handle}/publish`);
    // The panel is a region labelled by its "Publish" h2 (aria-labelledby). Scope
    // to it so the CTA never collides with the page-header "Publish" button, which
    // shares the same accessible name but lives outside this region.
    const panel = page.getByRole("region", { name: "Publish" });
    await expect(
      panel.getByRole("heading", { name: "Publish", level: 2 })
    ).toBeVisible();

    // There is a draft change to publish (the seeded index.html), so the Publish
    // CTA is present. Click it, then confirm in the AlertDialog.
    const publishCta = panel.getByRole("button", { name: "Publish", exact: true });
    await expect(publishCta).toBeEnabled();
    await publishCta.click();

    // Confirm dialog: an AlertDialog with its own "Publish" action button.
    const dialog = page.getByRole("alertdialog");
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "Publish", exact: true }).click();

    // Success: the toast fires AND the panel's current-status flips to published
    // (no more changes to publish). Assert on the durable state, not just the
    // ephemeral toast: the "No changes to publish" empty state confirms the draft
    // is now level with published.
    await expect(
      page.getByText("No changes to publish")
    ).toBeVisible({ timeout: 30_000 });
  });

  test("4. the published content is served on the site's subdomain", async ({
    page,
  }) => {
    // No auth needed: published sites are public (KOTOJI_PUBLISHED_PUBLIC=true).
    // Chromium resolves *.localhost to loopback, so <handle>.<base domain> hits
    // the same single-origin edge, which routes the host to the serve plane.
    const url = siteUrl(handle);
    const response = await page.goto(url, { waitUntil: "domcontentloaded" });
    expect(response, `no response from ${url}`).not.toBeNull();
    expect(response!.ok(), `served ${url} -> ${response!.status()}`).toBeTruthy();

    // The unique marker from the seed index.html is on the page.
    await expect(page.getByText(marker, { exact: true })).toBeVisible();
  });

  test("5. issue a per-user MCP/API token on /settings", async ({ page }) => {
    await loginAsAdmin(page);

    await page.goto("/settings");
    // The token panel is labelled by its heading "MCP / API tokens".
    const panel = page.getByRole("region", { name: "MCP / API tokens" });
    await expect(panel).toBeVisible();

    const tokenName = `e2e-${Date.now().toString(36)}`;
    // Required field: label renders "Token name * (Required)"; anchor on prefix.
    await panel.getByLabel(/^Token name/).fill(tokenName);

    // Default scopes (read+write) are pre-checked; ensure Publish too so the
    // token is interesting, then issue. Target the ARIA checkbox by ROLE: a
    // getByLabel("Publish") matches BOTH the custom checkbox span and the hidden
    // native <input> behind it (strict-mode violation), whereas the checkbox role
    // resolves to the single interactive control.
    await panel.getByRole("checkbox", { name: "Publish" }).check();
    await panel.getByRole("button", { name: "Issue token" }).click();

    // Show-once dialog reveals the plaintext token exactly once.
    const showOnce = page.getByRole("dialog", { name: "Copy your token" });
    await expect(showOnce).toBeVisible({ timeout: 15_000 });
    // The MCP config snippet in the dialog carries a Bearer token — a strong
    // signal the secret was minted (the prefix is the documented token shape).
    await expect(showOnce.getByText(/Bearer\s+\S+/)).toBeVisible();
    // Two controls are named "Close": the footer action button AND the dialog's
    // top-right "✕" (sr-only "Close"). Take the first — the explicit footer action
    // — to dismiss without a strict-mode collision.
    await showOnce.getByRole("button", { name: "Close" }).first().click();

    // The issued token now appears in the user's token list by name.
    await expect(panel.getByText(tokenName, { exact: true })).toBeVisible();
  });
});

// A tiny guard so a misconfigured base domain fails loudly with a clear message
// rather than as a confusing navigation timeout deep in a step.
test("environment is configured for the single-origin edge", async () => {
  expect(
    BASE_DOMAIN,
    "KOTOJI_E2E_BASE_DOMAIN must be the edge's KOTOJI_BASE_DOMAIN"
  ).toBeTruthy();
  expect(ADMIN_PASSWORD.length, "admin password must be >= 8 chars").toBeGreaterThanOrEqual(8);
});
