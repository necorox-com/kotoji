"use client";

/**
 * History hooks: log (commit history), diff (two refs / working tree), rollback
 * (restore an ancestor tree as a NEW forward commit), and commit (the multi-file
 * "Save" batch verb). All optimistic-locked writes carry baseSha and surface a
 * typed ConflictError on 409 (CANONICAL.md §1 Rollback/Commit).
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type {
  CommitInfo,
  DiffResult,
  LogResult,
  WriteResult,
} from "../types";

/** Commit history for a branch (newest first). Refetch on focus for liveness. */
export function useLog(
  handle: string,
  branch: string,
  opts?: { limit?: number; before?: string; path?: string }
) {
  return useQuery<LogResult>({
    queryKey: queryKeys.site(handle).log(branch),
    queryFn: () =>
      call(() =>
        apiClient.GET("/api/sites/{handle}/branches/{branch}/log", {
          params: {
            path: { handle, branch },
            query: {
              ...(opts?.limit ? { limit: opts.limit } : {}),
              ...(opts?.before ? { before: opts.before } : {}),
              ...(opts?.path ? { path: opts.path } : {}),
            },
          },
        })
      ),
    enabled: handle.length > 0 && branch.length > 0,
    // History is a status-like list — fresh on focus is desirable (design §4.2).
    refetchOnWindowFocus: true,
  });
}

/**
 * Diff two refs. When `to` is omitted the diff is against the working tree of
 * `from`'s branch (used by PublishPanel draft↔published preview, design §3.3).
 */
export function useDiff(
  handle: string,
  from: string,
  to?: string,
  opts?: { path?: string; contextLines?: number; nameStatus?: boolean }
) {
  return useQuery<DiffResult>({
    queryKey: queryKeys.site(handle).diff(from, to ?? ""),
    queryFn: () =>
      call(() =>
        apiClient.GET("/api/sites/{handle}/diff", {
          params: {
            path: { handle },
            query: {
              from,
              ...(to ? { to } : {}),
              ...(opts?.path ? { path: opts.path } : {}),
              ...(opts?.contextLines !== undefined
                ? { contextLines: opts.contextLines }
                : {}),
              ...(opts?.nameStatus !== undefined
                ? { nameStatus: opts.nameStatus }
                : {}),
            },
          },
        })
      ),
    enabled: handle.length > 0 && from.length > 0,
  });
}

/** Restore a branch to an ancestor commit's tree (a new forward commit). */
export function useRollback(handle: string, branch: string) {
  const qc = useQueryClient();
  return useMutation<
    CommitInfo,
    Error,
    { toSha: string; baseSha: string; message?: string }
  >({
    mutationFn: ({ toSha, baseSha, message }) =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/branches/{branch}/rollback", {
          params: { path: { handle, branch } },
          body: { toSha, baseSha, ...(message ? { message } : {}) },
        })
      ),
    onSuccess: () => {
      const site = queryKeys.site(handle);
      qc.invalidateQueries({ queryKey: site.log(branch) });
      qc.invalidateQueries({ queryKey: site.files(branch) });
      qc.invalidateQueries({ queryKey: site.detail() });
    },
  });
}

/**
 * Commit ("Save") the already-staged working set as one commit — the multi-file
 * batch verb (Monaco "save all"). Optimistic-locked on baseSha.
 */
export function useCommit(handle: string, branch: string) {
  const qc = useQueryClient();
  return useMutation<WriteResult, Error, { baseSha: string; message?: string }>({
    mutationFn: ({ baseSha, message }) =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/branches/{branch}/commit", {
          params: { path: { handle, branch } },
          body: { baseSha, ...(message ? { message } : {}) },
        })
      ),
    onSuccess: () => {
      const site = queryKeys.site(handle);
      qc.invalidateQueries({ queryKey: site.log(branch) });
      qc.invalidateQueries({ queryKey: site.files(branch) });
      qc.invalidateQueries({ queryKey: site.detail() });
    },
  });
}
