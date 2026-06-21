"use client";

/**
 * Breadcrumbs (organism) — design.md §3.3 (TopNav hosts a breadcrumb) and the
 * page wireframes (e.g. "ダッシュボード / expense-calc"). A thin, app-aware
 * wrapper over the ui/breadcrumb primitive: it takes a list of crumbs, renders
 * all but the last as Next links and the last as the current page, and collapses
 * the middle into an ellipsis when there are too many segments (keeps the bar on
 * one line on phones). Long handles/paths truncate with `…` per §2.3.
 */

import * as React from "react";
import {
  Breadcrumb,
  BreadcrumbEllipsis,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";
import { Link } from "@/components/atoms";
import { cn } from "@/lib/utils";

export interface BreadcrumbCrumb {
  /** Visible label (already localized by the caller). */
  label: React.ReactNode;
  /** Target route; omit on the current (last) page. */
  href?: string;
}

export interface BreadcrumbsProps {
  items: BreadcrumbCrumb[];
  /**
   * Max crumbs shown before the middle collapses into an ellipsis. The first
   * and last are always kept (most useful context on a narrow screen).
   */
  maxItems?: number;
  className?: string;
}

export function Breadcrumbs({
  items,
  maxItems = 3,
  className,
}: BreadcrumbsProps) {
  if (items.length === 0) return null;

  // Collapse the middle when there are more crumbs than we can comfortably show
  // on a phone. We always keep the first crumb (root) and the last (current).
  const collapsed =
    items.length > maxItems
      ? [items[0], { label: "ellipsis" as const }, items[items.length - 1]]
      : items;

  return (
    <Breadcrumb className={cn("min-w-0", className)}>
      <BreadcrumbList className="flex-nowrap">
        {collapsed.map((item, index) => {
          const isLast = index === collapsed.length - 1;
          const key = index;

          // The synthetic ellipsis marker injected by the collapse step above.
          if ("label" in item && item.label === "ellipsis" && !("href" in item)) {
            return (
              <React.Fragment key={`ellipsis-${key}`}>
                <BreadcrumbItem>
                  <BreadcrumbEllipsis />
                </BreadcrumbItem>
                <BreadcrumbSeparator />
              </React.Fragment>
            );
          }

          const crumb = item as BreadcrumbCrumb;

          return (
            <React.Fragment key={key}>
              <BreadcrumbItem className="min-w-0">
                {isLast || !crumb.href ? (
                  <BreadcrumbPage className="max-w-[12rem] truncate">
                    {crumb.label}
                  </BreadcrumbPage>
                ) : (
                  <BreadcrumbLink
                    render={
                      <Link
                        href={crumb.href}
                        variant="muted"
                        className="max-w-[10rem] truncate"
                      />
                    }
                  >
                    {crumb.label}
                  </BreadcrumbLink>
                )}
              </BreadcrumbItem>
              {!isLast ? <BreadcrumbSeparator /> : null}
            </React.Fragment>
          );
        })}
      </BreadcrumbList>
    </Breadcrumb>
  );
}
