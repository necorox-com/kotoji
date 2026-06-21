"use client";

/**
 * useCopyToClipboard — copy text and expose a transient `copied` flag (design.md
 * §3.2 CopyableUrl: icon → Check for ~1.5s, toast "コピーしました"). Falls back to
 * a hidden-textarea + execCommand when the async Clipboard API is unavailable
 * (insecure contexts / older browsers).
 */

import { useCallback, useEffect, useRef, useState } from "react";

const COPIED_RESET_MS = 1500;

export interface UseCopyResult {
  copied: boolean;
  /** Returns true on success so callers can toast/announce accordingly. */
  copy: (text: string) => Promise<boolean>;
}

export function useCopyToClipboard(resetMs = COPIED_RESET_MS): UseCopyResult {
  const [copied, setCopied] = useState(false);
  // Track the reset timer so rapid re-copies don't leave a stale timeout.
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Clear any pending reset on unmount to avoid setState after unmount.
  useEffect(() => {
    return () => {
      if (timer.current) clearTimeout(timer.current);
    };
  }, []);

  const copy = useCallback(
    async (text: string): Promise<boolean> => {
      let ok = false;
      try {
        if (navigator?.clipboard?.writeText) {
          await navigator.clipboard.writeText(text);
          ok = true;
        } else {
          // Fallback for insecure contexts where the async API is blocked.
          const el = document.createElement("textarea");
          el.value = text;
          el.style.position = "fixed";
          el.style.opacity = "0";
          document.body.appendChild(el);
          el.select();
          ok = document.execCommand("copy");
          document.body.removeChild(el);
        }
      } catch {
        ok = false;
      }

      if (ok) {
        setCopied(true);
        if (timer.current) clearTimeout(timer.current);
        timer.current = setTimeout(() => setCopied(false), resetMs);
      }
      return ok;
    },
    [resetMs]
  );

  return { copied, copy };
}
