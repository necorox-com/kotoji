"use client";

/**
 * Site lifecycle hooks: list, get, create, rename, delete (+settings update).
 * Mutations invalidate the relevant query keys so lists/detail stay coherent
 * (design.md §4.2). Every mutation surfaces a typed ApiError on failure for the
 * caller's onError toast.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type {
  CachePurgeResult,
  CreateSiteRequest,
  MirrorResult,
  Site,
  SiteSummary,
  UpdateSiteRequest,
} from "../types";

/** List sites visible to the caller (owned or member). */
export function useSites() {
  return useQuery<SiteSummary[]>({
    queryKey: queryKeys.sites(),
    queryFn: async () => {
      const res = await call(() => apiClient.GET("/api/sites"));
      return res.sites;
    },
  });
}

/** Get one site's detail by current handle. `enabled` guards empty handles. */
export function useSite(handle: string) {
  return useQuery<Site>({
    queryKey: queryKeys.site(handle).detail(),
    queryFn: () =>
      call(() =>
        apiClient.GET("/api/sites/{handle}", {
          params: { path: { handle } },
        })
      ),
    enabled: handle.length > 0,
  });
}

/** Create a site. On success the list is invalidated so the new card appears. */
export function useCreateSite() {
  const qc = useQueryClient();
  return useMutation<Site, Error, CreateSiteRequest>({
    mutationFn: (body) =>
      call(() => apiClient.POST("/api/sites", { body })),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.sites() });
    },
  });
}

/** Rename a site's handle (owner only). Records a redirect server-side. */
export function useRenameSite(handle: string) {
  const qc = useQueryClient();
  return useMutation<Site, Error, { newHandle: string }>({
    mutationFn: (body) =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/rename", {
          params: { path: { handle } },
          body,
        })
      ),
    onSuccess: (site) => {
      // The handle changed: drop the old scoped cache, refresh list + new detail.
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).all });
      qc.invalidateQueries({ queryKey: queryKeys.sites() });
      qc.setQueryData(queryKeys.site(site.handle).detail(), site);
    },
  });
}

/** Update site settings (visibility, publishMode, description, webRoot). */
export function useUpdateSite(handle: string) {
  const qc = useQueryClient();
  return useMutation<Site, Error, UpdateSiteRequest>({
    mutationFn: (body) =>
      call(() =>
        apiClient.PATCH("/api/sites/{handle}", {
          params: { path: { handle } },
          body,
        })
      ),
    onSuccess: (site) => {
      qc.setQueryData(queryKeys.site(handle).detail(), site);
      qc.invalidateQueries({ queryKey: queryKeys.sites() });
    },
  });
}

/**
 * Manually trigger a GitHub mirror push of draft + published (owner only).
 *
 * The endpoint is best-effort by contract: it returns 200 in EVERY non-access
 * outcome (not linked / remote unreachable / success). Therefore `call()` only
 * throws on the access-control codes (401/403/404); the {ok,message} body for
 * the not-linked / push-failed cases arrives as a normal resolved MirrorResult
 * that the caller surfaces via toast. On a successful push the site detail is
 * refreshed (publishedSha/updatedAt may have advanced server-side).
 */
export function useMirror(handle: string) {
  const qc = useQueryClient();
  return useMutation<MirrorResult, Error, void>({
    mutationFn: () =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/mirror", {
          params: { path: { handle } },
        })
      ),
    onSuccess: (result) => {
      if (result.ok) {
        qc.invalidateQueries({ queryKey: queryKeys.site(handle).detail() });
      }
    },
  });
}

/**
 * Purge the served cache for a site (editor or owner) by bumping its per-site
 * cache version, which is folded into every served asset's ETag. Published
 * changes already propagate immediately (assets are served `no-cache`); this
 * action additionally forces every client to refetch fresh on their next
 * revalidation — without requiring a new publish/commit. Returns the NEW
 * cacheVersion. The site detail is refreshed so any version-derived view stays
 * coherent (mirrors the other site action hooks).
 */
export function usePurgeCache(handle: string) {
  const qc = useQueryClient();
  return useMutation<CachePurgeResult, Error, void>({
    mutationFn: () =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/cache/purge", {
          params: { path: { handle } },
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).detail() });
    },
  });
}

/** Soft-delete a site (owner only). Invalidates the list. */
export function useDeleteSite() {
  const qc = useQueryClient();
  return useMutation<void, Error, { handle: string }>({
    mutationFn: ({ handle }) =>
      call(() =>
        apiClient.DELETE("/api/sites/{handle}", {
          params: { path: { handle } },
        })
      ),
    onSuccess: (_data, { handle }) => {
      qc.removeQueries({ queryKey: queryKeys.site(handle).all });
      qc.invalidateQueries({ queryKey: queryKeys.sites() });
    },
  });
}
