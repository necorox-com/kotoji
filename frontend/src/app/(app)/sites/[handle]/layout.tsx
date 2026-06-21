/**
 * ProjectDetail segment layout — wraps every nested section (Files/Branches/
 * Publish/History/Members/Settings) in the ProjectDetailLayout chrome (design.md
 * §3.4.3). The layout persists across section navigations (App Router nested
 * layouts), so the TopNav + BranchBar + section tabs don't re-mount when the user
 * switches sections — only the slotted section content changes.
 *
 * The full-bleed-vs-framed content decision is made INSIDE the layout from the
 * active section (Files → full-bleed split-pane), so this file just threads the
 * handle through.
 */

import type { ReactNode } from "react";
import { ProjectDetailLayout } from "@/components/templates";

// Local route-params type: matches Next's async `params` (a Promise) for this
// dynamic segment. Declared explicitly so `tsc --noEmit` is self-contained and
// doesn't depend on Next's generated `.next/types` globals (LayoutProps), which
// only exist after `next dev`/`next build`/`next typegen`.
type SiteLayoutProps = {
  children: ReactNode;
  params: Promise<{ handle: string }>;
};

export default async function SiteLayout({
  children,
  params,
}: SiteLayoutProps) {
  const { handle } = await params;
  return (
    <ProjectDetailLayout handle={handle}>{children}</ProjectDetailLayout>
  );
}
