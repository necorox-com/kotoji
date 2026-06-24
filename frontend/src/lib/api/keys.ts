/**
 * TanStack Query key factory — the single place query keys are constructed so
 * invalidation is consistent and refactor-safe (design.md §4.2).
 *
 * Convention (design.md §4.2):
 *   ["sites"], ["site", handle], ["files", handle, branch],
 *   ["file", handle, branch, path], ["log", handle, branch], ["diff", ...],
 *   ["me"], ["config"], etc.
 *
 * Each factory returns a readonly tuple (`as const`) so keys are tuple-typed.
 * `all`/scoped helpers let callers invalidate broad sub-trees in one call, e.g.
 * `queryClient.invalidateQueries({ queryKey: queryKeys.site(handle).all })`.
 */

export const queryKeys = {
  // -------- auth / instance --------
  me: () => ["me"] as const,
  config: () => ["config"] as const,

  // -------- admin (instance-wide settings) --------
  // Instance GitHub mirror config (admin-only; secret-safe view).
  adminGitHub: () => ["admin", "github"] as const,

  // Instance domain/URL config (admin-only): effective base domain + control
  // base URL with their source ("env"|"db"|"derived") and env-locked flags.
  adminDomain: () => ["admin", "domain"] as const,

  // Instance OIDC (Google sign-in) config (admin-only; secret-safe view): the
  // effective provider config with per-field source/locked flags + the effective
  // enabled auth-provider set.
  adminOIDC: () => ["admin", "oidc"] as const,

  // -------- tokens (per-user, account-level) --------
  // The current user's MCP/API tokens (membership-capped; span all the user's
  // projects). Owned by the user, not a site, so this lives at the top level
  // (CANONICAL §6: tokens are per-user, not per-project).
  tokens: () => ["tokens"] as const,

  // -------- sites --------
  sites: () => ["sites"] as const,

  /**
   * Per-site scoped keys. `.all` is the broad prefix to invalidate everything
   * about one site (detail, files, branches, history, members).
   */
  site: (handle: string) => {
    const root = ["site", handle] as const;
    return {
      all: root,
      detail: () => [...root, "detail"] as const,

      branches: () => [...root, "branches"] as const,

      // Files listing for one branch, optionally under a dir / at a ref.
      files: (branch: string, dir = "", ref = "") =>
        [...root, "files", branch, dir, ref] as const,

      // One file's content (the lock token `sha` lives in the payload).
      file: (branch: string, path: string, ref = "") =>
        [...root, "file", branch, path, ref] as const,

      // Commit history for a branch.
      log: (branch: string) => [...root, "log", branch] as const,

      // Diff between two refs (or a ref and its working tree when `to` empty).
      diff: (from: string, to = "") => [...root, "diff", from, to] as const,

      members: () => [...root, "members"] as const,
    };
  },
} as const;

export type QueryKeys = typeof queryKeys;
