"use client";

/**
 * useMediaQuery / useBreakpoint — SSR-safe responsive helpers (design.md §4.10).
 *
 * Implemented with useSyncExternalStore (the React 19 idiom for subscribing to
 * an external store), which is SSR-safe via its server snapshot and avoids the
 * set-state-in-effect anti-pattern. The server snapshot is `false` (mobile-first
 * default), then the real match takes over on the client after hydration.
 *
 * Prefer CSS (`lg:` etc.) for pure layout; use these only where a component
 * genuinely renders differently (Monaco diff side-by-side vs unified, FileTree
 * inline vs drawer). The three canonical bands map to design.md §2.8:
 *   isPhone   (<640)  · isTablet (640–1023) · isDesktop (≥1024)
 */

import { useCallback, useSyncExternalStore } from "react";

export function useMediaQuery(query: string): boolean {
  // subscribe: register a change listener on the MediaQueryList; React calls
  // getSnapshot again whenever this fires.
  const subscribe = useCallback(
    (onChange: () => void) => {
      if (typeof window === "undefined" || !window.matchMedia) {
        return () => {};
      }
      const mql = window.matchMedia(query);
      mql.addEventListener("change", onChange);
      return () => mql.removeEventListener("change", onChange);
    },
    [query]
  );

  // Client snapshot: the live match. Guarded for environments without matchMedia.
  const getSnapshot = useCallback(() => {
    if (typeof window === "undefined" || !window.matchMedia) return false;
    return window.matchMedia(query).matches;
  }, [query]);

  // Server snapshot: mobile-first default (avoids hydration mismatch).
  const getServerSnapshot = () => false;

  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

// Canonical breakpoint pixel values (design.md §2.8).
const PHONE_MAX = 639; // < 640
const DESKTOP_MIN = 1024; // >= lg

/**
 * useBreakpoint — the three named bands as booleans. Exactly one of
 * isPhone/isTablet/isDesktop is true once mounted (phone is the SSR default).
 */
export function useBreakpoint() {
  const isPhone = useMediaQuery(`(max-width: ${PHONE_MAX}px)`);
  const isDesktop = useMediaQuery(`(min-width: ${DESKTOP_MIN}px)`);
  // Tablet is the middle band; on the server everything is false so phone (the
  // mobile-first default) is the baseline by inverting the others.
  const isTablet = !isPhone && !isDesktop;
  return { isPhone, isTablet, isDesktop };
}
