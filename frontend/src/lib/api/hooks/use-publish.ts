"use client";

/**
 * usePublish — promote a source branch (default "draft") to "published"
 * (CANONICAL.md §1 Publish). Optimistic-locked on baseSha (the tip of `from`).
 *
 * Two distinct 409s reach `mutation.error` as typed errors (see error.ts):
 *  - ConflictError        : stale baseSha (the source moved under us).
 *  - PublishConflictError : a merge conflict promoting into published.
 * PublishPanel branches on these to show the right plain-language explanation
 * (design.md §3.3 PublishPanel / §3.5 Publish wireframe).
 *
 * On success we invalidate the site detail (publish badge), branches (heads),
 * and the published diff so the "下書きが先行" state reconciles.
 */

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type { PublishResult } from "../types";

export interface PublishArgs {
  /** Source branch tip to publish; defaults to "draft" server-side. */
  from?: string;
  /** REQUIRED: tip of `from` the publish is intended for (optimistic lock). */
  baseSha: string;
  message?: string;
}

export function usePublish(handle: string) {
  const qc = useQueryClient();
  return useMutation<PublishResult, Error, PublishArgs>({
    mutationFn: ({ from, baseSha, message }) =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/publish", {
          params: { path: { handle } },
          body: {
            // openapi marks `from` with a default of "draft"; send explicitly so
            // the typed body is complete and intent is unambiguous.
            from: from ?? "draft",
            baseSha,
            ...(message ? { message } : {}),
          },
        })
      ),
    onSuccess: (result) => {
      const site = queryKeys.site(handle);
      qc.invalidateQueries({ queryKey: site.detail() });
      qc.invalidateQueries({ queryKey: site.branches() });
      // The draft↔published diff is now empty/changed — refresh it.
      qc.invalidateQueries({ queryKey: site.diff(result.from, "published") });
      qc.invalidateQueries({ queryKey: queryKeys.sites() });
    },
  });
}
