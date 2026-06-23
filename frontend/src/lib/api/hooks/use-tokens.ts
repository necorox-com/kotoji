"use client";

/**
 * Per-USER MCP/API token hooks (CANONICAL §6, re-architected model).
 *
 * A token is owned by the CURRENT USER — not a project. One token carries one
 * scope set ⊆ {read,write,publish} (+ canCreateSites) and automatically covers
 * every project the user is a member of; the effective scope on a given site is
 * intersection(token.scopes, membership-role scopes), re-evaluated server-side
 * on every MCP request (membership-capped). These hooks therefore live at the
 * account level (queryKeys.tokens()), not under a site.
 *
 *  - list (never returns the secret/hash; prefix + metadata only),
 *  - create (the plaintext `token` is returned EXACTLY ONCE — the UI must
 *    surface it in a copy-once dialog and never refetch it),
 *  - revoke (owner-scoped: you can only revoke your own tokens).
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type { CreatedToken, CreateTokenRequest, TokenSummary } from "../types";

/** List the current user's tokens (prefix + metadata only; never the secret). */
export function useTokens() {
  return useQuery<TokenSummary[]>({
    queryKey: queryKeys.tokens(),
    queryFn: async () => {
      const res = await call(() => apiClient.GET("/api/tokens"));
      return res.tokens;
    },
  });
}

/**
 * Issue a token for the current user. The response includes the plaintext
 * `token` exactly once (CreatedToken). The UI must surface it in a copy-once
 * dialog and never refetch it. We invalidate the list so the new prefix appears.
 */
export function useCreateToken() {
  const qc = useQueryClient();
  return useMutation<CreatedToken, Error, CreateTokenRequest>({
    mutationFn: (body) =>
      call(() => apiClient.POST("/api/tokens", { body })),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.tokens() });
    },
  });
}

/** Revoke one of the current user's own tokens. */
export function useRevokeToken() {
  const qc = useQueryClient();
  return useMutation<void, Error, { tokenId: string }>({
    mutationFn: ({ tokenId }) =>
      call(() =>
        apiClient.DELETE("/api/tokens/{tokenId}", {
          params: { path: { tokenId } },
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.tokens() });
    },
  });
}
