"use client";

/**
 * Token hooks: list (never returns the secret), create (plaintext shown ONCE),
 * revoke. Owner-only (CANONICAL.md §6.1). Drives the "Connect AI / MCP" panel
 * (design.md §5 gap #3): show-once copy of the new token, then list/revoke.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type { CreatedToken, CreateTokenRequest, TokenSummary } from "../types";

/** List a site's tokens (prefix + metadata only; never the secret/hash). */
export function useTokens(handle: string) {
  return useQuery<TokenSummary[]>({
    queryKey: queryKeys.site(handle).tokens(),
    queryFn: async () => {
      const res = await call(() =>
        apiClient.GET("/api/sites/{handle}/tokens", {
          params: { path: { handle } },
        })
      );
      return res.tokens;
    },
    enabled: handle.length > 0,
  });
}

/**
 * Issue a site token. The response includes the plaintext `token` exactly once
 * (CreatedToken). The UI must surface it in a copy-once dialog and never refetch
 * it. We invalidate the list so the new prefix appears.
 */
export function useCreateToken(handle: string) {
  const qc = useQueryClient();
  return useMutation<CreatedToken, Error, CreateTokenRequest>({
    mutationFn: (body) =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/tokens", {
          params: { path: { handle } },
          body,
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).tokens() });
    },
  });
}

/** Revoke a token (owner only). */
export function useRevokeToken(handle: string) {
  const qc = useQueryClient();
  return useMutation<void, Error, { tokenId: string }>({
    mutationFn: ({ tokenId }) =>
      call(() =>
        apiClient.DELETE("/api/sites/{handle}/tokens/{tokenId}", {
          params: { path: { handle, tokenId } },
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).tokens() });
    },
  });
}
