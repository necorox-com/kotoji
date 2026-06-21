"use client";

/**
 * Branch hooks: list, create (host-safe feature-<user>-<slug>), delete.
 * Non-engineer copy frames branches as "別バージョン / versions" (design.md
 * §3.2 BranchSelect), but the wire stays git-branch shaped.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type { Branch } from "../types";

/** List a site's branches (enumerated from git). */
export function useBranches(handle: string) {
  return useQuery<Branch[]>({
    queryKey: queryKeys.site(handle).branches(),
    queryFn: async () => {
      const res = await call(() =>
        apiClient.GET("/api/sites/{handle}/branches", {
          params: { path: { handle } },
        })
      );
      return res.branches;
    },
    enabled: handle.length > 0,
  });
}

/** Create a branch from a source ref (branch name or SHA). */
export function useCreateBranch(handle: string) {
  const qc = useQueryClient();
  return useMutation<Branch, Error, { name: string; from: string }>({
    mutationFn: (body) =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/branches", {
          params: { path: { handle } },
          body,
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).branches() });
    },
  });
}

/** Delete a branch (server refuses published/draft). */
export function useDeleteBranch(handle: string) {
  const qc = useQueryClient();
  return useMutation<void, Error, { branch: string }>({
    mutationFn: ({ branch }) =>
      call(() =>
        apiClient.DELETE("/api/sites/{handle}/branches/{branch}", {
          params: { path: { handle, branch } },
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).branches() });
    },
  });
}
