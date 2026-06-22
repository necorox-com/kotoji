"use client";

/**
 * Admin (instance-wide) hooks — the instance GitHub mirror config (design.md
 * §3.5 Admin / instance Settings). ADMIN-ONLY: both endpoints sit behind
 * RequireAdmin server-side (401 anonymous / 403 non-admin), so the calling page
 * gates the form on `me.user.isAdmin` and only mounts these when admin.
 *
 * The config is SECRET-SAFE: GET never returns the token/webhook secret, only
 * `tokenSet`/`webhookSecretSet` "configured" booleans (CANONICAL: token is
 * write-only). PUT is a PARTIAL update — absent fields are left unchanged; an
 * empty/absent `token` keeps the stored one, `clearToken:true` removes it.
 *
 * On a successful update we invalidate BOTH the admin github query (refresh the
 * configured booleans) and the public ["config"] query — toggling `enabled`
 * flips `githubMirrorEnabled`, which the per-site GitHub panel reads.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type { GitHubAdminConfig, GitHubAdminConfigUpdate } from "../types";

/**
 * Read the EFFECTIVE instance GitHub mirror config (DB overrides env). Secret-
 * safe (token/webhook secret reduced to booleans). Admin-only: a non-admin hits
 * 403, so callers must only enable this when `me.user.isAdmin`.
 */
export function useAdminGitHub(enabled = true) {
  return useQuery<GitHubAdminConfig>({
    queryKey: queryKeys.adminGitHub(),
    queryFn: () => call(() => apiClient.GET("/api/admin/github")),
    enabled,
  });
}

/**
 * Update the instance GitHub mirror config (partial). On success we refresh the
 * admin view AND the public ["config"] (githubMirrorEnabled may have flipped, so
 * the per-site mirror panel reflects the new state).
 */
export function useUpdateAdminGitHub() {
  const qc = useQueryClient();
  return useMutation<GitHubAdminConfig, Error, GitHubAdminConfigUpdate>({
    mutationFn: (body) =>
      call(() => apiClient.PUT("/api/admin/github", { body })),
    onSuccess: (config) => {
      // Seed the admin view with the authoritative response, then invalidate
      // the public config so githubMirrorEnabled (and downstream UI) refreshes.
      qc.setQueryData(queryKeys.adminGitHub(), config);
      qc.invalidateQueries({ queryKey: queryKeys.config() });
    },
  });
}
