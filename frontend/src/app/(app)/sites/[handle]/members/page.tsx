"use client";

/**
 * ProjectDetail · Members — design.md §3.5 ProjectDetail · Members.
 *
 * Thin wrapper over MemberTable (which owns the list, invite form, role changes,
 * remove + confirm, the sole-owner guard, and the responsive table↔cards switch).
 * The caller's role gates member management (owner-only) inside the organism; we
 * thread it via useSiteRole.
 */

import { use } from "react";
import { MemberTable } from "@/components/organisms";
import { useSiteRole } from "@/hooks";

// Local route-params type (Next async `params`); see page.tsx for rationale.
type SiteParams = { params: Promise<{ handle: string }> };

export default function MembersPage({ params }: SiteParams) {
  const { handle } = use(params);
  const { role } = useSiteRole(handle);

  return <MemberTable handle={handle} role={role} />;
}
