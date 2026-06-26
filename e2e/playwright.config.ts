import { defineConfig, devices } from "@playwright/test";

/**
 * Playwright config for kotoji's core-journey E2E suite.
 *
 * The suite drives a REAL composed stack assembled as a SINGLE ORIGIN by the
 * Traefik edge overlay in HTTP mode (deploy/docker-compose.yml +
 * deploy/docker-compose.edge.yml, KOTOJI_BASE_DOMAIN=hosting.localhost). On that
 * origin the dashboard, /api, /auth and every served subdomain
 * (<handle>.hosting.localhost) all share http://hosting.localhost, so the browser
 * dashboard can reach /api same-origin and published sites are reachable by host.
 *
 * baseURL is read from KOTOJI_E2E_BASE_URL so the same tests run against a local
 * `docker compose ... up` box and against the CI-composed stack unchanged. It
 * defaults to the edge overlay's HTTP control host.
 *
 * The served-subdomain assertion relies on Chromium resolving *.localhost to the
 * loopback address (built-in, RFC 6761), so no /etc/hosts entry is needed inside
 * the browser. The CI health-gate (curl) uses an explicit Host header instead.
 */

// The single-origin control host. Trailing slash is intentionally omitted so
// page.goto("/dashboard") composes a clean URL.
const BASE_URL = process.env.KOTOJI_E2E_BASE_URL ?? "http://hosting.localhost";

// CI is detected via the standard env var GitHub Actions (and most CI) sets.
const IS_CI = !!process.env.CI;

export default defineConfig({
  testDir: "./tests",
  // One worker keeps the shared single-admin instance deterministic: the journey
  // mutates global instance state (first-run admin setup, sites), so parallel
  // specs racing on the same backend would be flaky. The suite is small and the
  // value is sequential coverage of the journey, not raw parallelism.
  workers: 1,
  fullyParallel: false,
  // Never allow an accidental `test.only` to silently green CI.
  forbidOnly: IS_CI,
  // One retry in CI absorbs transient first-paint / cold-cache hiccups without
  // masking a real regression (a test that only passes on retry still surfaces
  // in the report). Locally, fail fast with no retries.
  retries: IS_CI ? 1 : 0,
  // Per-test and per-assertion budgets sized for a cold container stack: the
  // first dashboard paint and the Monaco-free zip→publish path are comfortably
  // under these, with headroom for a slow CI runner.
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: IS_CI
    ? [["list"], ["html", { open: "never", outputFolder: "playwright-report" }]]
    : [["list"]],
  outputDir: "test-results",
  use: {
    baseURL: BASE_URL,
    // Deterministic English copy: the app picks its locale from Accept-Language
    // for first-time visitors (proxy.ts), and the NEXT_LOCALE=en cookie is set
    // per-context in the test fixtures. Together they pin English so visible-text
    // selectors are stable regardless of the runner's default locale.
    locale: "en-US",
    extraHTTPHeaders: { "Accept-Language": "en-US,en;q=0.9" },
    // Capture a trace on the first retry only — zero overhead on the happy path,
    // full timeline + DOM snapshots when something actually fails in CI.
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "off",
    // The action/navigation budgets gate individual clicks and gotos.
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
    // Plain-HTTP localhost edge: ignore any cert noise defensively (the default
    // overlay is HTTP, but this keeps the suite robust if pointed at a self-
    // signed TLS box via KOTOJI_E2E_BASE_URL).
    ignoreHTTPSErrors: true,
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
