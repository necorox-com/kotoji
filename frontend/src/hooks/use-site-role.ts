"use client";

/**
 * useSiteRole — resolve the CALLER's per-site role for capability gating.
 *
 * The Site DETAIL payload (GET /api/sites/{handle}) does not carry the caller's
 * role, but the LIST payload (SiteSummary, GET /api/sites) does (CANONICAL §6
 * role axis). We read it from the already-cached sites list (useSites) and fall
 * back to owner-by-ownerId when the summary isn't loaded yet but the detail is
 * (e.g. deep-linking straight into a site). Default is the most restrictive
 * "viewer" so the UI never offers an action the backend will 403.
 *
 * Capabilities are still re-checked server-side on every mutation; this is purely
 * UX gating (src/lib/api/capabilities.ts).
 */

import { useSites, useSite, useMe } from "@/lib/api/hooks";
import type { SiteRole } from "@/lib/api/types";

export interface UseSiteRoleResult {
  /** The caller's role on this site (defaults to "viewer" until resolved). */
  role: SiteRole;
  /** True while we still can't be sure of the role (both sources unresolved). */
  isLoading: boolean;
}

export function useSiteRole(handle: string): UseSiteRoleResult {
  const sites = useSites();
  const site = useSite(handle);
  const me = useMe();

  // 1) Prefer the authoritative summary role from the list.
  const summary = sites.data?.find((s) => s.handle === handle);
  if (summary) {
    return { role: summary.role, isLoading: false };
  }

  // 2) Fall back to ownership: if the detail's ownerId is me, I'm the owner.
  if (site.data && me.data && site.data.ownerId === me.data.user.id) {
    return { role: "owner", isLoading: false };
  }

  // 3) Otherwise default to the most restrictive role until we know better.
  const isLoading = sites.isLoading || site.isLoading || me.isLoading;
  return { role: "viewer", isLoading };
}
