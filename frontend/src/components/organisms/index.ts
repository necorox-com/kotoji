/**
 * Organisms barrel — navigation & dashboard-shell organisms (design.md §3.3).
 * Other organisms (file-tree, editor, publish, etc.) are exported from their
 * own files by their respective owners; import those directly.
 */

export { TopNav, BrandMark, type TopNavProps } from "./top-nav";
export {
  AppSidebar,
  AppSidebarSheet,
  SidebarNav,
  type AppSidebarSheetProps,
  type SidebarNavProps,
} from "./app-sidebar";
export { ProjectGrid, type ProjectGridProps } from "./project-grid";
export { CommandPalette, type CommandPaletteProps } from "./command-palette";
export { UserMenu, type UserMenuProps } from "./user-menu";
export {
  Breadcrumbs,
  type BreadcrumbsProps,
  type BreadcrumbCrumb,
} from "./breadcrumbs";

// Editor cluster (design.md §3.3): file tree, Monaco editor/diff, conflict
// resolution, zip upload.
export { FileTree, type FileTreeProps } from "./file-tree";
export {
  MonacoEditorPanel,
  type MonacoEditorPanelProps,
} from "./monaco-editor-panel";
export { DiffViewer, type DiffViewerProps } from "./diff-viewer";
export {
  ConflictResolver,
  type ConflictResolverProps,
} from "./conflict-resolver";
export { UploadDropzone, type UploadDropzoneProps } from "./upload-dropzone";

// Project-management cluster (design.md §3.3): branch bar, publish, history,
// members, create-site, MCP tokens.
export { BranchBar, type BranchBarProps } from "./branch-bar";
export { PublishPanel, type PublishPanelProps } from "./publish-panel";
export { HistoryTimeline, type HistoryTimelineProps } from "./history-timeline";
export { MemberTable, type MemberTableProps } from "./member-table";
export { CreateSiteForm, type CreateSiteFormProps } from "./create-site-form";
export { McpTokenPanel, type McpTokenPanelProps } from "./mcp-token-panel";
