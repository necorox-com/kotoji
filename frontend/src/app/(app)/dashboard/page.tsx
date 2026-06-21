"use client";

/**
 * Dashboard page (design.md §3.5 Dashboard).
 *
 * The site list. ProjectGrid owns the data + the loading/error/empty triplet +
 * the search/filter controls, so the page is just chrome: the DashboardLayout,
 * a page heading with the "新しいサイト" CTA, and the grid.
 */

import NextLink from "next/link";
import { useTranslations } from "next-intl";
import { Plus } from "lucide-react";
import { DashboardLayout } from "@/components/templates";
import { ProjectGrid } from "@/components/organisms";
import { SectionHeading } from "@/components/atoms";
import { Button } from "@/components/ui/button";

export default function DashboardPage() {
  const t = useTranslations("dashboard");
  const tNav = useTranslations("nav");

  const breadcrumbs = [{ label: tNav("dashboard") }];

  return (
    <DashboardLayout breadcrumbs={breadcrumbs}>
      <div className="space-y-6">
        <SectionHeading
          level={1}
          title={t("title")}
          actions={
            <Button render={<NextLink href="/sites/new" />}>
              <Plus aria-hidden="true" />
              {t("newSite")}
            </Button>
          }
        />
        <ProjectGrid />
      </div>
    </DashboardLayout>
  );
}
