"use client";

/**
 * ProjectCard (molecule) — design.md §3.2/§3.5. Dashboard tile: handle (title),
 * description, published StatusBadge + relative time, copyable URL, hover-lift;
 * the whole card links to detail. Built from ui/card + atoms. Takes a
 * SiteSummary (the list payload) so the grid never over-fetches.
 */

import { useFormatter, useTranslations } from "next-intl";
import { StatusBadge } from "@/components/atoms";
import { CopyableUrl } from "./copyable-url";
import { Link } from "@/components/atoms";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import type { SiteSummary } from "@/lib/api/types";
import { cn } from "@/lib/utils";

export interface ProjectCardProps {
  site: SiteSummary;
  /** Base domain for the preview URL (from InstanceConfig.baseDomain). */
  baseDomain?: string;
  className?: string;
}

export function ProjectCard({ site, baseDomain, className }: ProjectCardProps) {
  const t = useTranslations();
  const format = useFormatter();

  // Published vs draft drives the status chip (a11y: icon+text in StatusBadge).
  const status = site.hasPublished ? "published" : "draft";
  // The relative "2h ago" timestamp (next-intl handles locale + units).
  const updatedRelative = format.relativeTime(new Date(site.updatedAt));
  const previewUrl = baseDomain
    ? `${site.handle}.${baseDomain}`
    : site.handle;

  return (
    <Card
      className={cn(
        "group relative gap-3 transition-transform duration-100 hover:-translate-y-0.5 hover:shadow-md",
        className
      )}
    >
      <CardHeader className="gap-1.5">
        <div className="flex items-center justify-between gap-2">
          {/* The whole card is a link; this stretched link covers it while the
              copy button stays independently clickable (z-index in CopyableUrl). */}
          <CardTitle className="min-w-0 truncate">
            <Link
              href={`/sites/${site.handle}`}
              variant="nav"
              className="font-semibold before:absolute before:inset-0 before:z-0 before:content-['']"
            >
              {site.handle}
            </Link>
          </CardTitle>
          <StatusBadge status={status} className="relative z-10" />
        </div>
        {site.description ? (
          <CardDescription className="line-clamp-2">
            {site.description}
          </CardDescription>
        ) : null}
      </CardHeader>
      <CardContent className="flex items-center justify-between gap-2">
        {/* Copy must sit above the stretched link so clicking copy doesn't
            navigate (relative z-10). */}
        <div className="relative z-10 min-w-0 flex-1">
          <CopyableUrl value={previewUrl} />
        </div>
        <span className="shrink-0 text-xs text-muted-foreground">
          {t("site.lastUpdated")} {updatedRelative}
        </span>
      </CardContent>
    </Card>
  );
}
