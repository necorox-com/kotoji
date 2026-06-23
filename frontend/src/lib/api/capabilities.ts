/**
 * Role -> capability matrix (CANONICAL.md §6.1, FROZEN).
 *
 * The frontend uses this to drive which actions are SHOWN / ENABLED per the
 * caller's per-site role. The backend remains the authority (it re-checks every
 * mutation); this is purely UX gating so non-permitted actions never render as
 * dead buttons that 403.
 *
 * Two orthogonal axes exist in the contract; this module covers the per-site
 * ROLE axis (owner | editor | viewer). The token-SCOPE axis is enforced
 * server-side: under the re-architected model (CANONICAL §6) a per-user token's
 * EFFECTIVE scope on a site is intersection(token scopes, the user's membership-
 * role scopes), so there is NO per-site "manage tokens" capability — token
 * issuance is account-level (AccountTokenPanel on /settings).
 *
 * Pure data + functions (no React) so it is unit-testable and reusable by any
 * organism. Imported by BranchBar, PublishPanel, HistoryTimeline, MemberTable.
 */

import type { PublishMode, SiteRole } from "./types";

/** The set of gateable site capabilities (a subset of the §6.1 matrix rows). */
export type Capability =
  | "read" // read files / history / diff / log / previews
  | "write" // write/save files, create/delete branches, upload zip, rollback
  | "publish" // publish (direct) / request publish
  | "rename" // rename handle
  | "deleteSite" // soft-delete the site
  | "manageMembers" // add/remove/role members
  | "manageSettings"; // visibility / publish_mode / web_root / GitHub mirror

/**
 * CANONICAL.md §6.1, transcribed. `true` means the role is granted the row.
 * NOTE: "publish" here means "may publish at all" (direct or request); the
 * direct-vs-request distinction is `publish_mode`, handled by canPublish().
 */
const ROLE_CAPS: Record<SiteRole, Record<Capability, boolean>> = {
  owner: {
    read: true,
    write: true,
    publish: true,
    rename: true,
    deleteSite: true,
    manageMembers: true,
    manageSettings: true,
  },
  editor: {
    read: true,
    write: true,
    publish: true,
    rename: false,
    deleteSite: false,
    manageMembers: false,
    manageSettings: false,
  },
  viewer: {
    read: true,
    write: false,
    publish: false,
    rename: false,
    deleteSite: false,
    manageMembers: false,
    manageSettings: false,
  },
};

/** Does this role have the given capability? (CANONICAL.md §6.1). */
export function roleCan(role: SiteRole, capability: Capability): boolean {
  return ROLE_CAPS[role]?.[capability] ?? false;
}

/**
 * May this role PUBLISH given the site's publish_mode?
 *
 * Per §6.1 notes: owners always publish; editors publish DIRECTLY under
 * publish_mode='direct'. Under publish_mode='request', non-owners route through
 * a publish request (still allowed to *initiate* — the button just changes copy
 * to "公開をリクエスト"). Viewers never publish.
 *
 * This returns whether the publish CTA should be ENABLED at all; the CTA's
 * LABEL (publish vs request) is chosen from publish_mode + role by the caller.
 */
export function canPublish(role: SiteRole, mode: PublishMode): boolean {
  // Both owners and editors may initiate publish; request-mode only reshapes
  // the action (PR delegation) but never removes the affordance for editors.
  // `mode` is part of the signature so callers always thread the site's mode
  // through one decision point; it does not gate WHETHER publish is offered
  // (that is purely the role), only HOW (see isPublishRequest).
  void mode;
  return roleCan(role, "publish");
}

/**
 * Is the publish a "request" (PR-delegated) rather than a direct publish for
 * this role + mode? Owners always publish directly; editors under 'request'
 * mode go through a request.
 */
export function isPublishRequest(role: SiteRole, mode: PublishMode): boolean {
  if (role === "owner") return false;
  return mode === "request";
}
