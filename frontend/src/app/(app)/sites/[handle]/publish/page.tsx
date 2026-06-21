"use client";

/**
 * ProjectDetail · Publish — design.md §3.5 ProjectDetail · Publish.
 *
 * Thin wrapper over the PublishPanel organism (which owns the diff preview,
 * publish message, confirm + mutation, conflict handling, and role/mode gating).
 * The page threads in the caller's role (useSiteRole), the site's publish mode,
 * the active "version" to publish (from `?branch=`, default draft), and the base
 * domain for the published URL.
 */

import { use } from "react";
import { useSearchParams } from "next/navigation";
import { PublishPanel } from "@/components/organisms";
import { useConfig } from "@/lib/api/hooks";
import { useSiteRole } from "@/hooks";
import type { PublishMode } from "@/lib/api/types";

const DEFAULT_BRANCH = "draft";
const DEFAULT_BASE_DOMAIN = "hosting.example.com";

// Local route-params type (Next async `params`); see page.tsx for rationale.
type SiteParams = { params: Promise<{ handle: string }> };

export default function PublishPage({ params }: SiteParams) {
  const { handle } = use(params);
  const searchParams = useSearchParams();
  const branch = searchParams.get("branch") ?? DEFAULT_BRANCH;

  const { data: config } = useConfig();
  const { role } = useSiteRole(handle);

  const publishMode: PublishMode = config?.defaultPublishMode ?? "direct";
  const baseDomain = config?.baseDomain ?? DEFAULT_BASE_DOMAIN;

  return (
    <PublishPanel
      handle={handle}
      from={branch}
      role={role}
      publishMode={publishMode}
      baseDomain={baseDomain}
    />
  );
}
