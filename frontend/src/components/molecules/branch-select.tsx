"use client";

/**
 * BranchSelect (molecule) — design.md §3.2. Switch the active branch ("version"
 * in non-engineer copy). Shows each branch with a status dot (published→success,
 * draft→neutral, feature-*→info) and a "新しいバージョンを作成" action at the
 * bottom. Emits onValueChange / onCreateNew; data comes via props (no fetching).
 */

import { Plus } from "lucide-react";
import { useTranslations } from "next-intl";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Branch } from "@/lib/api/types";
import { cn } from "@/lib/utils";

// Sentinel value for the "create new" row so it never collides with a branch.
const CREATE_NEW = "__create_new__";

export interface BranchSelectProps {
  branches: Branch[];
  value: string;
  onValueChange: (branch: string) => void;
  onCreateNew?: () => void;
  className?: string;
  "aria-label"?: string;
}

/** Map a branch to its status-dot color (semantic, paired with text label). */
function dotClass(branch: Branch): string {
  if (branch.isPublished) return "bg-success";
  if (branch.name === "draft") return "bg-muted-foreground";
  return "bg-info";
}

export function BranchSelect({
  branches,
  value,
  onValueChange,
  onCreateNew,
  className,
  ...rest
}: BranchSelectProps) {
  const t = useTranslations("branches");

  const handleChange = (next: string | null) => {
    if (next == null) return;
    if (next === CREATE_NEW) {
      onCreateNew?.();
      return;
    }
    onValueChange(next);
  };

  return (
    <Select value={value} onValueChange={handleChange}>
      <SelectTrigger
        className={cn("min-w-44", className)}
        aria-label={rest["aria-label"] ?? t("current")}
      >
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {branches.map((branch) => (
          <SelectItem key={branch.name} value={branch.name}>
            <span
              className={cn("size-2 shrink-0 rounded-full", dotClass(branch))}
              aria-hidden="true"
            />
            <span className="truncate">{branch.name}</span>
          </SelectItem>
        ))}
        {onCreateNew ? (
          <>
            <SelectSeparator />
            <SelectItem value={CREATE_NEW}>
              <Plus className="size-4" aria-hidden="true" />
              <span>{t("newVersion")}</span>
            </SelectItem>
          </>
        ) : null}
      </SelectContent>
    </Select>
  );
}
