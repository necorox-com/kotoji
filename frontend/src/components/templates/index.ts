/**
 * Templates barrel (design.md §3.4). Layout skeletons + responsive behavior;
 * pages slot content in.
 */

export { AuthLayout, type AuthLayoutProps } from "./auth-layout";
export { AuthGate, type AuthGateProps } from "./auth-gate";
export {
  DashboardLayout,
  type DashboardLayoutProps,
} from "./dashboard-layout";
export {
  ProjectDetailLayout,
  type ProjectDetailLayoutProps,
} from "./project-detail-layout";
export {
  AdminLayout,
  ADMIN_SECTIONS,
  type AdminLayoutProps,
  type AdminSection,
} from "./admin-layout";
