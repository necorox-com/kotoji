/**
 * (app) group layout — authenticated, guarded routes (design.md §3.5 / §4.3).
 *
 * Applies ONLY the client-side AuthGate here (redirect-to-login on 401). The
 * visual chrome differs per page: Dashboard/CreateSite/Admin use DashboardLayout,
 * while ProjectDetail uses ProjectDetailLayout (split-pane). So each page wraps
 * itself in the right template; this group just guards the whole subtree.
 */

import { AuthGate } from "@/components/templates";

export default function AppGroupLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return <AuthGate>{children}</AuthGate>;
}
