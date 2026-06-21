"use client";

/**
 * Member hooks: list, add/upsert by email, change role, remove (owner-only per
 * CANONICAL.md §6.1). Drives MemberTable / MemberRow / RoleSelect (design §3.2).
 */

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiClient, call } from "../client";
import { queryKeys } from "../keys";
import type { Member, SiteRole } from "../types";

/** List a site's members + roles. */
export function useMembers(handle: string) {
  return useQuery<Member[]>({
    queryKey: queryKeys.site(handle).members(),
    queryFn: async () => {
      const res = await call(() =>
        apiClient.GET("/api/sites/{handle}/members", {
          params: { path: { handle } },
        })
      );
      return res.members;
    },
    enabled: handle.length > 0,
  });
}

/** Add or upsert a member by email. */
export function useAddMember(handle: string) {
  const qc = useQueryClient();
  return useMutation<Member, Error, { email: string; role: SiteRole }>({
    mutationFn: (body) =>
      call(() =>
        apiClient.POST("/api/sites/{handle}/members", {
          params: { path: { handle } },
          body,
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).members() });
    },
  });
}

/** Change a member's role. */
export function useUpdateMemberRole(handle: string) {
  const qc = useQueryClient();
  return useMutation<Member, Error, { userId: string; role: SiteRole }>({
    mutationFn: ({ userId, role }) =>
      call(() =>
        apiClient.PATCH("/api/sites/{handle}/members/{userId}", {
          params: { path: { handle, userId } },
          body: { role },
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).members() });
    },
  });
}

/** Remove a member (server refuses removing the sole owner -> 409). */
export function useRemoveMember(handle: string) {
  const qc = useQueryClient();
  return useMutation<void, Error, { userId: string }>({
    mutationFn: ({ userId }) =>
      call(() =>
        apiClient.DELETE("/api/sites/{handle}/members/{userId}", {
          params: { path: { handle, userId } },
        })
      ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: queryKeys.site(handle).members() });
    },
  });
}
