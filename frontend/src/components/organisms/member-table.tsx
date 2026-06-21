"use client";

/**
 * MemberTable (organism) — design.md §3.3 / §3.5 ProjectDetail · Members.
 *
 * Members CRUD: list members with an inline role Select, an invite form
 * (email + role), and a remove action behind a ConfirmDialog. ALL mutations are
 * OWNER-ONLY (CANONICAL §6.1: "Manage members" = owner). Viewers/editors see a
 * read-only table.
 *
 * Server guards the sole-owner case (removing/demoting the only owner -> 409);
 * we ALSO disable those affordances client-side for the only owner so the UI
 * never offers a button that will fail (honest-state, design §1.2 #4).
 *
 * Responsive: a real <table> on `sm+`; stacked cards on phones (the table would
 * overflow at 375px). Loading/error/empty triplet via the molecules.
 */

import { useMemo, useState } from "react";
import { UserPlus, Users } from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/molecules/confirm-dialog";
import { EmptyState } from "@/components/molecules/empty-state";
import { ErrorState } from "@/components/molecules/error-state";
import { LoadingState } from "@/components/molecules/loading-state";
import { Spinner } from "@/components/atoms";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  useMembers,
  useAddMember,
  useUpdateMemberRole,
  useRemoveMember,
} from "@/lib/api/hooks";
import { errorMessage } from "@/lib/api/error";
import { roleCan } from "@/lib/api/capabilities";
import type { Member, SiteRole } from "@/lib/api/types";
import { cn } from "@/lib/utils";

const ROLE_VALUES: SiteRole[] = ["owner", "editor", "viewer"];

export interface MemberTableProps {
  handle: string;
  /** The caller's role (owner unlocks member management; CANONICAL §6.1). */
  role: SiteRole;
  className?: string;
}

/** Two-letter initials for the Avatar fallback. */
function initials(name: string, email: string): string {
  const base = name.trim() || email.trim();
  if (!base) return "?";
  const parts = base.split(/\s+/);
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

/** A localized role Select shared by the table rows and the invite form. */
function RoleSelect({
  value,
  onChange,
  disabled,
  label,
}: {
  value: SiteRole;
  onChange: (role: SiteRole) => void;
  disabled?: boolean;
  label: string;
}) {
  const tRoles = useTranslations("roles");
  return (
    <Select
      value={value}
      onValueChange={(v) => v != null && onChange(v as SiteRole)}
      disabled={disabled}
    >
      <SelectTrigger aria-label={label} className="min-w-28" size="sm">
        <SelectValue>{(v: SiteRole) => tRoles(v)}</SelectValue>
      </SelectTrigger>
      <SelectContent>
        {ROLE_VALUES.map((r) => (
          <SelectItem key={r} value={r}>
            {tRoles(r)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

export function MemberTable({ handle, role, className }: MemberTableProps) {
  const t = useTranslations("members");
  const tRoles = useTranslations("roles");

  const membersQuery = useMembers(handle);
  const addMember = useAddMember(handle);
  const updateRole = useUpdateMemberRole(handle);
  const removeMember = useRemoveMember(handle);

  const canManage = roleCan(role, "manageMembers");

  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState<SiteRole>("editor");
  const [pendingRemove, setPendingRemove] = useState<Member | null>(null);

  // Stabilize the derived array so the ownerCount memo's deps stay referentially
  // stable across renders (a fresh `?? []` literal would change every render).
  const members = useMemo(() => membersQuery.data ?? [], [membersQuery.data]);

  // Sole-owner guard: if exactly one owner remains, its role can't be changed
  // away and it can't be removed (server would 409; we pre-disable).
  const ownerCount = useMemo(
    () => members.filter((m) => m.role === "owner").length,
    [members],
  );
  const isLockedOwner = (m: Member) => m.role === "owner" && ownerCount <= 1;

  const submitInvite = async () => {
    const email = inviteEmail.trim();
    if (!email) return;
    try {
      await addMember.mutateAsync({ email, role: inviteRole });
      toast.success(t("added"));
      setInviteEmail("");
      setInviteRole("editor");
    } catch (err) {
      toast.error(errorMessage(err, t("loadError")));
    }
  };

  const changeRole = async (m: Member, next: SiteRole) => {
    if (next === m.role) return;
    try {
      await updateRole.mutateAsync({ userId: m.userId, role: next });
      toast.success(t("roleUpdated"));
    } catch (err) {
      toast.error(errorMessage(err, t("cannotRemoveSoleOwner")));
    }
  };

  const confirmRemove = async () => {
    if (!pendingRemove) return;
    try {
      await removeMember.mutateAsync({ userId: pendingRemove.userId });
      toast.success(t("removed"));
      setPendingRemove(null);
    } catch (err) {
      toast.error(errorMessage(err, t("cannotRemoveSoleOwner")));
    }
  };

  return (
    <section
      data-slot="member-table"
      className={cn("space-y-5", className)}
      aria-labelledby="members-heading"
    >
      <h2
        id="members-heading"
        className="text-xl font-semibold text-foreground"
      >
        {t("title")}
      </h2>

      {/* Invite form (owner only) */}
      {canManage ? (
        <form
          className="flex flex-col gap-3 rounded-lg border border-border bg-card p-3 sm:flex-row sm:items-end"
          onSubmit={(e) => {
            e.preventDefault();
            void submitInvite();
          }}
        >
          <div className="grid flex-1 gap-1.5">
            <Label htmlFor="invite-email">{t("inviteEmail")}</Label>
            <Input
              id="invite-email"
              type="email"
              value={inviteEmail}
              onChange={(e) => setInviteEmail(e.target.value)}
              placeholder="name@example.com"
              autoComplete="off"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="invite-role">{t("role")}</Label>
            <RoleSelect
              value={inviteRole}
              onChange={setInviteRole}
              label={t("role")}
            />
          </div>
          <Button
            type="submit"
            disabled={addMember.isPending || inviteEmail.trim().length === 0}
            aria-busy={addMember.isPending}
          >
            {addMember.isPending ? (
              <Spinner size="sm" />
            ) : (
              <UserPlus aria-hidden="true" />
            )}
            {t("invite")}
          </Button>
        </form>
      ) : null}

      {/* loading / error / empty / content */}
      {membersQuery.isLoading ? (
        <LoadingState rows={3} label={t("title")} />
      ) : membersQuery.isError ? (
        <ErrorState
          error={membersQuery.error}
          title={t("loadError")}
          onRetry={() => membersQuery.refetch()}
        />
      ) : members.length === 0 ? (
        <EmptyState
          icon={Users}
          title={t("empty.title")}
          body={t("empty.body")}
        />
      ) : (
        <>
          {/* Desktop / tablet: table */}
          <div className="hidden sm:block">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("title")}</TableHead>
                  <TableHead>{t("role")}</TableHead>
                  {canManage ? (
                    <TableHead className="w-px text-right">
                      <span className="sr-only">{t("remove")}</span>
                    </TableHead>
                  ) : null}
                </TableRow>
              </TableHeader>
              <TableBody>
                {members.map((m) => {
                  const locked = isLockedOwner(m);
                  return (
                    <TableRow key={m.userId}>
                      <TableCell>
                        <div className="flex items-center gap-3">
                          <Avatar size="sm">
                            <AvatarFallback>
                              {initials(m.displayName, m.email)}
                            </AvatarFallback>
                          </Avatar>
                          <div className="min-w-0">
                            <p className="truncate font-medium text-foreground">
                              {m.displayName || m.email}
                            </p>
                            <p className="truncate text-xs text-muted-foreground">
                              {m.email}
                            </p>
                          </div>
                        </div>
                      </TableCell>
                      <TableCell>
                        {canManage ? (
                          <RoleSelect
                            value={m.role}
                            onChange={(next) => changeRole(m, next)}
                            disabled={locked || updateRole.isPending}
                            label={t("role")}
                          />
                        ) : (
                          <span className="text-sm text-muted-foreground">
                            {tRoles(m.role)}
                          </span>
                        )}
                      </TableCell>
                      {canManage ? (
                        <TableCell className="text-right">
                          <Button
                            variant="ghost"
                            size="sm"
                            disabled={locked}
                            onClick={() => setPendingRemove(m)}
                          >
                            {t("remove")}
                          </Button>
                        </TableCell>
                      ) : null}
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>

          {/* Phone: stacked cards */}
          <ul className="space-y-2 sm:hidden">
            {members.map((m) => {
              const locked = isLockedOwner(m);
              return (
                <li
                  key={m.userId}
                  className="rounded-lg border border-border bg-card p-3"
                >
                  <div className="flex items-center gap-3">
                    <Avatar size="sm">
                      <AvatarFallback>
                        {initials(m.displayName, m.email)}
                      </AvatarFallback>
                    </Avatar>
                    <div className="min-w-0 flex-1">
                      <p className="truncate font-medium text-foreground">
                        {m.displayName || m.email}
                      </p>
                      <p className="truncate text-xs text-muted-foreground">
                        {m.email}
                      </p>
                    </div>
                  </div>
                  <div className="mt-3 flex items-center justify-between gap-2">
                    {canManage ? (
                      <RoleSelect
                        value={m.role}
                        onChange={(next) => changeRole(m, next)}
                        disabled={locked || updateRole.isPending}
                        label={t("role")}
                      />
                    ) : (
                      <span className="text-sm text-muted-foreground">
                        {tRoles(m.role)}
                      </span>
                    )}
                    {canManage ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={locked}
                        onClick={() => setPendingRemove(m)}
                      >
                        {t("remove")}
                      </Button>
                    ) : null}
                  </div>
                </li>
              );
            })}
          </ul>
        </>
      )}

      {/* Remove confirm */}
      <ConfirmDialog
        open={pendingRemove !== null}
        onOpenChange={(open) => {
          if (!open) setPendingRemove(null);
        }}
        variant="destructive"
        title={t("removeConfirmTitle")}
        description={t("removeConfirmBody", {
          name: pendingRemove?.displayName || pendingRemove?.email || "",
        })}
        confirmLabel={t("remove")}
        onConfirm={confirmRemove}
        loading={removeMember.isPending}
      />
    </section>
  );
}
