"use client";

/**
 * SourceTag (molecule) — design.md §3.2. Provenance of a change:
 * Upload / Editor / MCP(AI) / System / GitHub. AI/MCP gets a distinct Sparkles
 * icon so AI-authored changes are legible at a glance. Maps the WriteSource enum
 * (CANONICAL.md §8 via) to an icon + i18n label.
 */

import {
  GitBranch,
  PenLine,
  Server,
  Sparkles,
  UploadCloud,
  type LucideIcon,
} from "lucide-react";
import { useTranslations } from "next-intl";
import { Chip } from "@/components/atoms";
import type { WriteSource } from "@/lib/api/types";

// Allow the extra "github" provenance the UI shows even though the wire enum
// folds webhook/github into "system" (CANONICAL.md §8 via mapping).
export type SourceKind = WriteSource | "github";

const SOURCE_ICON: Record<SourceKind, LucideIcon> = {
  upload: UploadCloud,
  editor: PenLine,
  mcp: Sparkles,
  system: Server,
  github: GitBranch,
};

export interface SourceTagProps {
  source: SourceKind;
  className?: string;
}

export function SourceTag({ source, className }: SourceTagProps) {
  const t = useTranslations("source");
  const Icon = SOURCE_ICON[source];
  return (
    <Chip className={className}>
      <Icon
        className="size-3 text-muted-foreground"
        aria-hidden="true"
      />
      {t(source)}
    </Chip>
  );
}
