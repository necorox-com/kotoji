"use client";

/**
 * useUploadZip — import a .zip into a branch, REPLACING the tree as one commit
 * (CANONICAL.md §1 ImportZip). multipart/form-data with optimistic-lock baseSha
 * (omitted only for the initial seed of an empty branch).
 *
 * Why XMLHttpRequest instead of openapi-fetch here: the UploadDropzone needs a
 * real upload Progress bar (design.md §3.3 UploadDropzone), and `fetch` cannot
 * report upload progress. We hand-roll the request but keep:
 *  - credentials (cookies) via xhr.withCredentials,
 *  - CSRF double-submit via the X-CSRF-Token header (client.ts helpers),
 *  - typed success (CommitInfo) and typed errors (parseApiError),
 *  - the same query invalidations as the other write hooks.
 */

import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  CSRF_COOKIE,
  CSRF_HEADER,
  readCookie,
} from "../client";
import { queryKeys } from "../keys";
import { ApiError, networkError, parseApiError } from "../error";
import type { CommitInfo } from "../types";

const API_BASE = process.env.NEXT_PUBLIC_API_BASE_URL ?? "";

export interface UploadZipArgs {
  file: File;
  /** REQUIRED unless the branch has no commits yet (initial seed). */
  baseSha?: string;
  message?: string;
  /** Progress callback 0..1 for the UploadDropzone Progress bar. */
  onProgress?: (fraction: number) => void;
  /**
   * Per-call handle override. The hook normally binds the handle at call time,
   * but the create-site flow only learns the new handle AFTER create() resolves
   * — and React state set in the same tick is not yet visible to the hook's
   * closure. Passing the handle here makes the import target the right site in
   * that same tick (otherwise the URL is built with an empty handle → 404 and
   * the seed silently never lands). Falls back to the hook-bound handle.
   */
  handle?: string;
}

/**
 * Perform the multipart upload via XHR so progress can be observed. Resolves to
 * the typed CommitInfo on 2xx; rejects with a typed ApiError otherwise.
 */
function uploadZipRequest(
  handle: string,
  branch: string,
  args: UploadZipArgs
): Promise<CommitInfo> {
  return new Promise<CommitInfo>((resolve, reject) => {
    const url = `${API_BASE}/api/sites/${encodeURIComponent(
      handle
    )}/branches/${encodeURIComponent(branch)}/import`;

    const form = new FormData();
    form.append("file", args.file);
    if (args.baseSha) form.append("baseSha", args.baseSha);
    if (args.message) form.append("message", args.message);

    const xhr = new XMLHttpRequest();
    xhr.open("POST", url, true);
    // Send the session cookie (mirrors openapi-fetch credentials:"include").
    xhr.withCredentials = true;
    // CSRF double-submit on this mutating request. In production (Secure
    // cookies) the backend names the cookie with the `__Host-` prefix; in
    // insecure dev it uses the bare name. Try both so the header is always set
    // (mirrors the openapi-fetch csrfMiddleware in client.ts).
    const csrf = readCookie(`__Host-${CSRF_COOKIE}`) || readCookie(CSRF_COOKIE);
    if (csrf) xhr.setRequestHeader(CSRF_HEADER, csrf);
    // Hint we accept JSON so the error envelope is returned as JSON.
    xhr.setRequestHeader("Accept", "application/json");

    // Upload progress for the Progress bar.
    if (args.onProgress) {
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) {
          args.onProgress?.(e.loaded / e.total);
        }
      };
    }

    xhr.onload = () => {
      // Build a Response-like object so parseApiError can classify by status.
      const parseBody = (): unknown => {
        try {
          return JSON.parse(xhr.responseText) as unknown;
        } catch {
          return undefined;
        }
      };
      if (xhr.status >= 200 && xhr.status < 300) {
        const body = parseBody();
        // 200 returns CommitInfo; if absent, surface an internal error.
        if (body) {
          resolve(body as CommitInfo);
        } else {
          reject(new ApiError(xhr.status, "internal", "空のレスポンスが返りました"));
        }
        return;
      }
      // Non-2xx: synthesize a Response carrying the status for parseApiError.
      const fakeResponse = {
        ok: false,
        status: xhr.status,
        statusText: xhr.statusText,
      } as Response;
      reject(parseApiError(fakeResponse, parseBody()));
    };

    // Transport failure (offline, CORS, aborted connection).
    xhr.onerror = () => reject(networkError());
    xhr.ontimeout = () => reject(networkError());

    xhr.send(form);
  });
}

/** Upload a zip to a branch. Invalidates files + log + site detail on success. */
export function useUploadZip(handle: string, branch: string) {
  const qc = useQueryClient();
  return useMutation<CommitInfo, Error, UploadZipArgs>({
    // Prefer the per-call handle override (create-site flow) over the bound one.
    mutationFn: (args) => uploadZipRequest(args.handle || handle, branch, args),
    onSuccess: (_data, args) => {
      const site = queryKeys.site(args.handle || handle);
      qc.invalidateQueries({ queryKey: site.files(branch) });
      qc.invalidateQueries({ queryKey: site.log(branch) });
      qc.invalidateQueries({ queryKey: site.detail() });
    },
  });
}
