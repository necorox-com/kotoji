/**
 * Convenience re-exports of the generated schema component types so app code
 * imports friendly names (`Site`, `Member`, `CommitInfo`, …) instead of the
 * verbose `components["schemas"]["Site"]` indexing everywhere. These are pure
 * type aliases over the GENERATED schema — no hand-maintained duplicate DTOs
 * (design.md §4.1). Update openapi.yaml + regenerate, and these follow.
 */

import type { components } from "./schema";

type S = components["schemas"];

// -------- enums / scalars --------
export type Handle = S["HandleString"];
export type BranchName = S["BranchString"];
export type Sha = S["Sha"];
export type SiteRole = S["SiteRole"];
export type SiteVisibility = S["SiteVisibility"];
export type PublishMode = S["PublishMode"];
export type TokenScope = S["TokenScope"];
export type WriteSource = S["WriteSource"];
export type AuthMode = S["AuthMode"];

// -------- auth / instance --------
export type Me = S["Me"];
export type User = S["User"];
export type InstanceConfig = S["InstanceConfig"];

// -------- sites --------
export type Site = S["Site"];
export type SiteSummary = S["SiteSummary"];
export type CreateSiteRequest = S["CreateSiteRequest"];
export type UpdateSiteRequest = S["UpdateSiteRequest"];

// -------- members --------
export type Member = S["Member"];

// -------- tokens --------
export type TokenSummary = S["TokenSummary"];
export type CreateTokenRequest = S["CreateTokenRequest"];
export type CreatedToken = S["CreatedToken"];

// -------- branches --------
export type Branch = S["Branch"];

// -------- files --------
export type FileEntry = S["FileEntry"];
export type FileListing = S["FileListing"];
export type FileContent = S["FileContent"];
export type WriteFileRequest = S["WriteFileRequest"];
export type WriteResult = S["WriteResult"];

// -------- history --------
export type CommitInfo = S["CommitInfo"];
export type CommitRequest = S["CommitRequest"];
export type PublishRequest = S["PublishRequest"];
export type PublishResult = S["PublishResult"];
export type DiffResult = S["DiffResult"];
export type FileDiff = S["FileDiff"];
export type LogResult = S["LogResult"];
export type RollbackRequest = S["RollbackRequest"];
