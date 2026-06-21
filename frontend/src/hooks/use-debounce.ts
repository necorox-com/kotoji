"use client";

/**
 * useDebounce — debounce a changing value (design.md §3.2 SearchBar 250ms,
 * CreateSite handle uniqueness 400ms). Returns the value only after it has been
 * stable for `delayMs`.
 */

import { useEffect, useState } from "react";

export function useDebounce<T>(value: T, delayMs = 250): T {
  const [debounced, setDebounced] = useState<T>(value);

  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    // Reset the timer on every change so only the final value lands.
    return () => clearTimeout(id);
  }, [value, delayMs]);

  return debounced;
}
