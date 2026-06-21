"use client";

/**
 * useMe / useConfig — the auth + instance-config reads (design.md §4.3).
 *
 * `useMe` calls GET /api/me: 200 => { user, authMode }; 401 => ApiError(401)
 * which the route guard branches on to redirect to /auth/login (design.md §4.3).
 * `useConfig` calls the PUBLIC GET /api/config for upload limits / handle rules /
 * auth modes (CANONICAL gap §5.8 resolved by the config endpoint).
 */

import { useQuery } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import { isUnauthenticated } from "../error";
import type { InstanceConfig, Me } from "../types";

/** Current authenticated user + auth mode. 401 surfaces as an ApiError. */
export function useMe() {
  return useQuery<Me>({
    queryKey: queryKeys.me(),
    queryFn: () => call(() => apiClient.GET("/api/me")),
    // Session rarely changes within a tab; keep it fresh for 5 minutes
    // (design.md §4.2: ["me"] longer staleTime).
    staleTime: 5 * 60_000,
    // Do NOT retry an auth failure — it will never succeed without re-login.
    retry: (failureCount, error) =>
      !isUnauthenticated(error) && failureCount < 1,
  });
}

/** Public-safe instance config (upload limits, handle rules, auth modes). */
export function useConfig() {
  return useQuery<InstanceConfig>({
    queryKey: queryKeys.config(),
    queryFn: () => call(() => apiClient.GET("/api/config")),
    // Instance config is effectively static for a session.
    staleTime: 30 * 60_000,
  });
}
