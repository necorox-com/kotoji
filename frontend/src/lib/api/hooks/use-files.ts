"use client";

/**
 * File read/write hooks with optimistic-lock conflict handling (design.md §4 /
 * CANONICAL.md §1 WriteFile, §3 ConflictError).
 *
 * The flow (design.md §4.1/§4.2):
 *  - useFileContent reads a file; the returned `sha` (COMMIT sha) is the lock
 *    token the editor MUST echo as `baseSha` on the next write.
 *  - useWriteFile sends `{ path, content, baseSha }`. A stale baseSha => the
 *    server returns 409 with the frozen ConflictError detail; `call()` throws a
 *    typed `ConflictError` which reaches `mutation.error`. We DELIBERATELY do
 *    NOT optimistically apply the write — the ConflictResolver decides.
 *  - On success we invalidate file content + listing + log + site detail so the
 *    new tip SHA (next baseSha) and dirty state are coherent.
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type {
  CommitInfo,
  FileContent,
  FileEntry,
  WriteResult,
} from "../types";

/** List files under a directory of a branch (optionally recursive / at a ref). */
export function useFiles(
  handle: string,
  branch: string,
  opts?: { dir?: string; ref?: string; recursive?: boolean }
) {
  const dir = opts?.dir ?? "";
  const ref = opts?.ref ?? "";
  return useQuery<FileEntry[]>({
    queryKey: queryKeys.site(handle).files(branch, dir, ref),
    queryFn: async () => {
      const res = await call(() =>
        apiClient.GET("/api/sites/{handle}/branches/{branch}/files", {
          params: {
            path: { handle, branch },
            query: {
              dir,
              ...(ref ? { ref } : {}),
              recursive: opts?.recursive ?? false,
            },
          },
        })
      );
      return res.files;
    },
    enabled: handle.length > 0 && branch.length > 0,
  });
}

/**
 * Read one file. The payload's `sha` is the optimistic-lock token; capture it at
 * open time and pass it back as `baseSha` on save (design.md §4.6). Disable
 * window-focus refetch so it never clobbers an open editor (design.md §4.2).
 */
export function useFileContent(
  handle: string,
  branch: string,
  path: string,
  ref?: string
) {
  return useQuery<FileContent>({
    queryKey: queryKeys.site(handle).file(branch, path, ref ?? ""),
    queryFn: () =>
      call(() =>
        apiClient.GET("/api/sites/{handle}/branches/{branch}/file", {
          params: {
            path: { handle, branch },
            query: { path, ...(ref ? { ref } : {}) },
          },
        })
      ),
    enabled: handle.length > 0 && branch.length > 0 && path.length > 0,
    refetchOnWindowFocus: false,
  });
}

/** Arguments for a single optimistic-locked write. */
export interface WriteFileArgs {
  path: string;
  content: string;
  /** REQUIRED: the COMMIT sha the edit is based on (from useFileContent.sha). */
  baseSha: string;
  encoding?: "utf-8" | "base64";
  /** Defaults to true at the edge (commit immediately). */
  commit?: boolean;
  message?: string;
}

/**
 * Write (save) one file. On a 409 the error is a typed `ConflictError`
 * (instanceof check in the EditorToolbar drives ConflictResolver). On success
 * we invalidate the file, its listing, the log, and the site detail so the next
 * baseSha (the new tip) is fresh.
 */
export function useWriteFile(handle: string, branch: string) {
  const qc = useQueryClient();
  return useMutation<WriteResult, Error, WriteFileArgs>({
    mutationFn: ({ path, content, baseSha, encoding, commit, message }) =>
      call(() =>
        apiClient.PUT("/api/sites/{handle}/branches/{branch}/file", {
          params: { path: { handle, branch } },
          body: {
            path,
            content,
            baseSha,
            // openapi marks encoding/commit with defaults; send them explicitly
            // so the request body is fully typed and intent is unambiguous.
            encoding: encoding ?? "utf-8",
            commit: commit ?? true,
            ...(message ? { message } : {}),
          },
        })
      ),
    onSuccess: (result) => {
      const site = queryKeys.site(handle);
      qc.invalidateQueries({ queryKey: site.file(branch, result.path) });
      qc.invalidateQueries({ queryKey: site.files(branch) });
      qc.invalidateQueries({ queryKey: site.log(branch) });
      qc.invalidateQueries({ queryKey: site.detail() });
    },
  });
}

/** Arguments for an optimistic-locked file delete. */
export interface DeleteFileArgs {
  path: string;
  baseSha: string;
  message?: string;
}

/** Delete one file (optimistic-locked; same 409 conflict semantics as write). */
export function useDeleteFile(handle: string, branch: string) {
  const qc = useQueryClient();
  return useMutation<CommitInfo, Error, DeleteFileArgs>({
    mutationFn: ({ path, baseSha, message }) =>
      call(() =>
        apiClient.DELETE("/api/sites/{handle}/branches/{branch}/file", {
          params: {
            path: { handle, branch },
            query: { path, baseSha, ...(message ? { message } : {}) },
          },
        })
      ),
    onSuccess: (_commit, { path }) => {
      const site = queryKeys.site(handle);
      qc.removeQueries({ queryKey: site.file(branch, path) });
      qc.invalidateQueries({ queryKey: site.files(branch) });
      qc.invalidateQueries({ queryKey: site.log(branch) });
      qc.invalidateQueries({ queryKey: site.detail() });
    },
  });
}
