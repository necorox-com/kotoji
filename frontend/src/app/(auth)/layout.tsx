/**
 * (auth) group layout — public, unauthenticated routes (design.md §3.5).
 * Wraps its pages (Login) in the centered AuthLayout chrome.
 */

import { AuthLayout } from "@/components/templates";

export default function AuthGroupLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return <AuthLayout>{children}</AuthLayout>;
}
