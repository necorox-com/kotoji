/**
 * Root route. kotoji has no marketing surface (self-hosted app); the entry point
 * is the dashboard. Authenticated users land there; the (app) layout's auth
 * guard (design.md §4.3) bounces unauthenticated visitors to /auth/login. So the
 * root simply forwards to /dashboard and lets the guard do its job.
 */

import { redirect } from "next/navigation";

export default function Home() {
  redirect("/dashboard");
}
