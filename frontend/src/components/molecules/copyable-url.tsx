"use client";

/**
 * CopyableUrl (molecule) — design.md §3.2. Shows a host/URL as mono CodeText
 * with a copy button; click copies, toasts "コピーしました", icon flips to Check
 * for ~1.5s. Truncates with a tooltip carrying the full value (§2.3 truncation).
 */

import { Check, Copy } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";
import { CodeText, IconButton } from "@/components/atoms";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useCopyToClipboard } from "@/hooks";
import { cn } from "@/lib/utils";

export interface CopyableUrlProps {
  /** The value to display + copy (e.g. handle.hosting.example.com). */
  value: string;
  /** Optional display override (defaults to value). */
  label?: string;
  /** Prefix copied/display with a scheme when opening externally. */
  href?: string;
  className?: string;
}

export function CopyableUrl({
  value,
  label,
  className,
}: CopyableUrlProps) {
  const t = useTranslations("common");
  const { copied, copy } = useCopyToClipboard();

  const onCopy = async () => {
    const ok = await copy(value);
    if (ok) {
      toast.success(t("copied"));
    } else {
      toast.error(t("copyFailed"));
    }
  };

  return (
    <div
      data-slot="copyable-url"
      className={cn("flex min-w-0 items-center gap-1", className)}
    >
      <Tooltip>
        <TooltipTrigger
          render={
            <CodeText truncate className="min-w-0 flex-1">
              {label ?? value}
            </CodeText>
          }
        />
        <TooltipContent>{value}</TooltipContent>
      </Tooltip>
      <IconButton
        size="icon-sm"
        aria-label={t("copy")}
        tooltip={copied ? t("copied") : t("copy")}
        onClick={onCopy}
      >
        {copied ? (
          <Check className="text-success" aria-hidden="true" />
        ) : (
          <Copy aria-hidden="true" />
        )}
      </IconButton>
    </div>
  );
}
