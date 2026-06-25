/**
 * useUploadZip — CSRF double-submit header regression test.
 *
 * Why this exists: the ImportZip upload is hand-rolled over XMLHttpRequest (to
 * get an upload Progress bar `fetch` cannot provide), so it does NOT go through
 * the openapi-fetch csrfMiddleware. It must therefore read the CSRF cookie with
 * the SAME `__Host-`-prefixed-first / bare-name-fallback rule as client.ts. In
 * production cookies are Secure and named `__Host-kotoji_csrf`; in insecure dev
 * they use the bare `kotoji_csrf`. A previous bug read only the bare name, so
 * uploads failed in production (no CSRF header => 403). These tests pin the
 * fallback order so that regression cannot return.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";

import { useUploadZip } from "./use-upload";
import { CSRF_COOKIE, CSRF_HEADER } from "../client";

// A minimal XMLHttpRequest stand-in that records the headers the hook sets and
// resolves with a 200 + CommitInfo body so the mutation completes successfully.
// Only the surface useUploadZip touches is implemented.
class MockXHR {
  static last: MockXHR | null = null;

  // Captured request headers keyed by header name.
  headers: Record<string, string> = {};
  withCredentials = false;
  status = 0;
  statusText = "";
  responseText = "";
  upload: { onprogress: ((e: ProgressEvent) => void) | null } = {
    onprogress: null,
  };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  ontimeout: (() => void) | null = null;

  constructor() {
    MockXHR.last = this;
  }

  open(): void {
    // No-op: method/url are irrelevant to the CSRF-header assertions.
  }

  setRequestHeader(name: string, value: string): void {
    this.headers[name] = value;
  }

  send(): void {
    // Simulate a successful import returning a CommitInfo envelope.
    this.status = 200;
    this.statusText = "OK";
    this.responseText = JSON.stringify({
      sha: "0".repeat(40),
      shortSha: "0000000",
      message: "import",
      authorName: "tester",
      authorEmail: "tester@example.com",
      committed: "2026-01-01T00:00:00Z",
      via: "ui",
    });
    // Fire onload asynchronously so the Promise in the hook settles like a real
    // network round-trip would.
    queueMicrotask(() => this.onload?.());
  }
}

// Force document.cookie to expose an exact cookie string. jsdom's setter
// REJECTS `__Host-`-prefixed cookies over http (the prefix mandates Secure +
// HTTPS), so we cannot reproduce the production cookie via assignment. The
// `__Host-` prefix only constrains how the SERVER may set the cookie, not its
// readability from JS — in the browser `document.cookie` exposes it normally.
// Stubbing the getter models exactly what the hook sees in production.
function setDocumentCookie(value: string): void {
  Object.defineProperty(document, "cookie", {
    configurable: true,
    get: () => value,
    // Swallow writes; tests only need the getter.
    set: () => {},
  });
}

// Provide a fresh QueryClient per test so cache state never leaks across cases.
function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return createElement(QueryClientProvider, { client: qc }, children);
}

// Drive the hook's mutation once and return the headers the mock XHR captured.
async function runUploadAndCaptureHeaders(): Promise<Record<string, string>> {
  const { result } = renderHook(() => useUploadZip("acme", "draft"), {
    wrapper,
  });
  result.current.mutate({
    file: new File(["zip-bytes"], "site.zip", { type: "application/zip" }),
    baseSha: "deadbeef",
  });
  await waitFor(() => expect(result.current.isSuccess).toBe(true));
  return MockXHR.last?.headers ?? {};
}

describe("useUploadZip CSRF cookie selection", () => {
  beforeEach(() => {
    MockXHR.last = null;
    vi.stubGlobal("XMLHttpRequest", MockXHR);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    // Restore the native document.cookie accessor between cases.
    setDocumentCookie("");
  });

  it("prefers the __Host- prefixed cookie (production, Secure cookies)", async () => {
    // Production exposes the prefixed name; also present the bare name with a
    // DIFFERENT value to prove the prefixed one wins.
    setDocumentCookie(
      `__Host-${CSRF_COOKIE}=prod-token; ${CSRF_COOKIE}=stale-bare-token`
    );

    const headers = await runUploadAndCaptureHeaders();

    expect(headers[CSRF_HEADER]).toBe("prod-token");
  });

  it("falls back to the bare cookie name (insecure dev)", async () => {
    // Dev only sets the bare name; the prefixed lookup must miss and fall back.
    setDocumentCookie(`${CSRF_COOKIE}=dev-token`);

    const headers = await runUploadAndCaptureHeaders();

    expect(headers[CSRF_HEADER]).toBe("dev-token");
  });

  it("omits the CSRF header when no cookie is present", async () => {
    // No cookie at all => no header set (server will reject; that is expected).
    setDocumentCookie("");

    const headers = await runUploadAndCaptureHeaders();

    expect(headers[CSRF_HEADER]).toBeUndefined();
  });
});
