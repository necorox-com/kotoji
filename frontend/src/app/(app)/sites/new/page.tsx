"use client";

/**
 * CreateSite page (design.md §3.5 CreateSite).
 *
 * Hosts CreateSiteForm in a single max-w-2xl column. The form does the create +
 * validation; this page wires the AFTER-create flow (CANONICAL §1 ImportZip is a
 * post-create op):
 *  - empty/template → redirect straight to the new ProjectDetail,
 *  - zip            → upload the chosen seed zip onto the new site's draft branch
 *    (initial seed → no baseSha), then redirect. We surface upload progress via a
 *    toast-less inline state and only navigate once the seed lands so the user
 *    arrives at a populated site.
 */

import { useState } from "react";
import { useRouter } from "next/navigation";
import { useTranslations } from "next-intl";
import { ArrowLeft } from "lucide-react";
import { toast } from "sonner";
import { DashboardLayout } from "@/components/templates";
import { CreateSiteForm } from "@/components/organisms";
import { SectionHeading } from "@/components/atoms";
import { Button } from "@/components/ui/button";
import { useUploadZip } from "@/lib/api/hooks";
import { errorMessage } from "@/lib/api/error";

/** The logical draft branch a new site is seeded onto (CANONICAL §5.2). */
const DRAFT_BRANCH = "draft";

export default function CreateSitePage() {
  const t = useTranslations("createSite");
  const tUpload = useTranslations("upload");
  const tNav = useTranslations("nav");
  const router = useRouter();

  // The uploader is bound to a (handle, branch); since the handle is only known
  // AFTER create, we keep it in state and (re)create the hook with the handle.
  const [seedHandle, setSeedHandle] = useState<string>("");
  const upload = useUploadZip(seedHandle, DRAFT_BRANCH);

  const breadcrumbs = [
    { label: tNav("dashboard"), href: "/dashboard" },
    { label: t("title") },
  ];

  const handleCreated = async (handle: string, seedZip?: File | null) => {
    // No seed → straight to the new site.
    if (!seedZip) {
      router.push(`/sites/${handle}`);
      return;
    }
    // Seed the new site's draft branch with the chosen zip (initial seed: no
    // baseSha), then navigate once it lands so the site is populated on arrival.
    setSeedHandle(handle);
    try {
      await upload.mutateAsync({ file: seedZip });
      toast.success(tUpload("success"));
    } catch (err) {
      // The site exists even if the seed failed; surface the error and still go
      // to the (empty) site so the user can re-upload there.
      toast.error(errorMessage(err, tUpload("error")));
    } finally {
      router.push(`/sites/${handle}`);
    }
  };

  return (
    <DashboardLayout breadcrumbs={breadcrumbs}>
      <div className="mx-auto max-w-2xl space-y-6">
        <SectionHeading
          level={1}
          title={t("title")}
          actions={
            <Button
              variant="ghost"
              size="sm"
              onClick={() => router.back()}
            >
              <ArrowLeft aria-hidden="true" />
              {tNav("dashboard")}
            </Button>
          }
        />
        <CreateSiteForm
          onCreated={handleCreated}
          onCancel={() => router.push("/dashboard")}
        />
      </div>
    </DashboardLayout>
  );
}
