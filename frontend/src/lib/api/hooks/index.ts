/**
 * Barrel for the typed API hooks. Organisms/pages import from here; they NEVER
 * call `apiClient` directly (design.md §4.2: hooks are the testable seam).
 */

export { useMe, useConfig } from "./use-me";
export {
  useSites,
  useSite,
  useCreateSite,
  useRenameSite,
  useUpdateSite,
  useMirror,
  useDeleteSite,
} from "./use-sites";
export {
  useFiles,
  useFileContent,
  useWriteFile,
  useDeleteFile,
  type WriteFileArgs,
  type DeleteFileArgs,
} from "./use-files";
export {
  useBranches,
  useCreateBranch,
  useDeleteBranch,
} from "./use-branches";
export {
  useLog,
  useDiff,
  useRollback,
  useCommit,
} from "./use-history";
export { usePublish, type PublishArgs } from "./use-publish";
export {
  useMembers,
  useAddMember,
  useUpdateMemberRole,
  useRemoveMember,
} from "./use-members";
export {
  useTokens,
  useCreateToken,
  useRevokeToken,
} from "./use-tokens";
export { useUploadZip, type UploadZipArgs } from "./use-upload";
