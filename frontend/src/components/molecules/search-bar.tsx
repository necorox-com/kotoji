"use client";

/**
 * SearchBar (molecule) — design.md §3.2. Input + Search icon + clear button +
 * "/" Kbd hint. Debounced onChange (default 250ms). Pressing "/" anywhere (when
 * not already typing) focuses it. Controlled value lives in the parent; we
 * surface the debounced value via onDebouncedChange.
 */

import { useEffect, useRef, useState } from "react";
import { Search, X } from "lucide-react";
import { useTranslations } from "next-intl";
import { Input } from "@/components/ui/input";
import { Kbd } from "@/components/atoms";
import { useDebounce } from "@/hooks";
import { cn } from "@/lib/utils";

export interface SearchBarProps {
  /** Debounced search-term callback. */
  onDebouncedChange?: (value: string) => void;
  placeholder?: string;
  debounceMs?: number;
  /** Enable the global "/" focus shortcut (default true). */
  globalShortcut?: boolean;
  className?: string;
  "aria-label"?: string;
}

export function SearchBar({
  onDebouncedChange,
  placeholder,
  debounceMs = 250,
  globalShortcut = true,
  className,
  ...rest
}: SearchBarProps) {
  const t = useTranslations("common");
  const [value, setValue] = useState("");
  const debounced = useDebounce(value, debounceMs);
  const inputRef = useRef<HTMLInputElement>(null);

  // Emit the debounced term whenever it settles.
  useEffect(() => {
    onDebouncedChange?.(debounced);
  }, [debounced, onDebouncedChange]);

  // "/" focuses the search unless the user is already typing in a field.
  useEffect(() => {
    if (!globalShortcut) return;
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      const typing =
        target &&
        (target.tagName === "INPUT" ||
          target.tagName === "TEXTAREA" ||
          target.isContentEditable);
      if (e.key === "/" && !typing) {
        e.preventDefault();
        inputRef.current?.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [globalShortcut]);

  return (
    <div
      data-slot="search-bar"
      className={cn("relative flex items-center", className)}
    >
      <Search
        className="pointer-events-none absolute left-2.5 size-4 text-muted-foreground"
        aria-hidden="true"
      />
      <Input
        ref={inputRef}
        type="search"
        value={value}
        onChange={(e) => setValue(e.target.value)}
        placeholder={placeholder ?? t("search")}
        aria-label={rest["aria-label"] ?? t("search")}
        className="pr-12 pl-8"
      />
      {value ? (
        <button
          type="button"
          onClick={() => {
            setValue("");
            inputRef.current?.focus();
          }}
          aria-label={t("clear")}
          className="absolute right-2.5 inline-flex size-4 items-center justify-center rounded-sm text-muted-foreground hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <X className="size-3.5" aria-hidden="true" />
        </button>
      ) : (
        <Kbd className="absolute right-2.5" aria-hidden="true">
          /
        </Kbd>
      )}
    </div>
  );
}
