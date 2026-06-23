"use client";

/**
 * AccountTokenPanel (organism) — the account-level "MCP / API トークン" section
 * on the /settings page (CANONICAL §6, re-architected model).
 *
 * Tokens are now PER-USER, not per-project: one token is owned by the current
 * user, carries one scope set ⊆ {read,write,publish} (+ optional create-site
 * capability), and AUTOMATICALLY covers every project the user is a member of.
 * The effective scope on a given site is intersection(token.scopes, the user's
 * membership-role scopes) re-evaluated server-side on every MCP request, so a
 * token can never exceed its user's own access (membership-capped). This panel
 * is therefore shown to EVERY authenticated user — these are the user's OWN
 * tokens.
 *
 * It:
 *  - lists existing tokens (prefix + metadata only; the secret is never re-shown;
 *    shows last-used + expiry),
 *  - issues a token with a name, scope selection (read ⊂ write ⊂ publish,
 *    CANONICAL §6.2), an optional expiry, and — only when the user themselves has
 *    `canCreateSites` — a "may create sites" toggle. The plaintext `token` is
 *    shown EXACTLY ONCE in a copy-once dialog (CreatedToken; useCreateToken),
 *  - shows an example MCP client config snippet wired to the new token + this
 *    instance's /mcp endpoint so a non-engineer can paste it into their AI tool,
 *  - revokes a token behind a ConfirmDialog.
 *
 * Scope capping is enforced SERVER-side (the effective scope can never exceed the
 * user's membership); the UI offers the chain and surfaces any rejection via
 * toast. Loading/error/empty triplet via the molecules. Mobile-first: token list
 * is stacked cards; the create form stacks; the show-once dialog scrolls its
 * snippet on narrow screens.
 */

import { useState, useSyncExternalStore } from "react";
import { KeyRound, Sparkles } from "lucide-react";
import { useFormatter, useTranslations } from "next-intl";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/molecules/confirm-dialog";
import { CopyableUrl } from "@/components/molecules/copyable-url";
import { EmptyState } from "@/components/molecules/empty-state";
import { ErrorState } from "@/components/molecules/error-state";
import { LoadingState } from "@/components/molecules/loading-state";
import { CodeText } from "@/components/atoms/code-text";
import { Chip } from "@/components/atoms/chip";
import { Spinner } from "@/components/atoms";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { useTokens, useCreateToken, useRevokeToken } from "@/lib/api/hooks";
import { errorMessage } from "@/lib/api/error";
import type {
  CreatedToken,
  TokenScope,
  TokenSummary,
} from "@/lib/api/types";
import { cn } from "@/lib/utils";

// The scope chain (read ⊂ write ⊂ publish; CANONICAL §6.2). Selecting a higher
// scope implies the lower ones server-side; we present them as a chain.
const SCOPE_VALUES: TokenScope[] = ["read", "write", "publish"];

// Placeholder origin used during SSR / before mount so the snippet renders
// stable markup; replaced with the real origin client-side.
const ORIGIN_PLACEHOLDER = "https://kotoji.example.com";

export interface AccountTokenPanelProps {
  /**
   * Whether the current user may create sites (me.user.canCreateSites). The
   * "may create sites" toggle is offered ONLY when true; the server caps the
   * requested capability to the user's own anyway.
   */
  canCreateSites: boolean;
  className?: string;
}

/** useMounted — true only after client mount (codebase idiom; UserMenu §4.4). */
function useMounted(): boolean {
  return useSyncExternalStore(
    () => () => {},
    () => true,
    () => false,
  );
}

/**
 * Build a copy-pasteable MCP client config snippet for the issued token. The MCP
 * endpoint is this instance's reserved `/mcp` path; the same token spans every
 * project the user is a member of (a `site` selector is supplied per MCP tool
 * call — see mcp.md), so no per-site config is needed here.
 */
function mcpConfigSnippet(token: string, endpoint: string): string {
  return JSON.stringify(
    {
      mcpServers: {
        kotoji: {
          url: endpoint,
          headers: {
            Authorization: `Bearer ${token}`,
          },
        },
      },
    },
    null,
    2,
  );
}

export function AccountTokenPanel({
  canCreateSites,
  className,
}: AccountTokenPanelProps) {
  const t = useTranslations("tokens");
  const tc = useTranslations("common");
  const format = useFormatter();

  const tokensQuery = useTokens();
  const createToken = useCreateToken();
  const revokeToken = useRevokeToken();

  const [name, setName] = useState("");
  const [scopes, setScopes] = useState<TokenScope[]>(["read", "write"]);
  const [expiresAt, setExpiresAt] = useState("");
  const [grantCreateSites, setGrantCreateSites] = useState(false);
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const [pendingRevoke, setPendingRevoke] = useState<TokenSummary | null>(null);

  // Live origin for the /mcp endpoint shown in the show-once snippet
  // (hydration-safe: placeholder during SSR, real origin after mount).
  const mounted = useMounted();
  const origin =
    mounted && typeof window !== "undefined"
      ? window.location.origin
      : ORIGIN_PLACEHOLDER;
  const endpoint = `${origin}/mcp`;

  const tokens = tokensQuery.data ?? [];

  const toggleScope = (scope: TokenScope, checked: boolean) => {
    setScopes((prev) => {
      const set = new Set(prev);
      if (checked) {
        set.add(scope);
        // Selecting a higher scope implies the lower ones (chain).
        const idx = SCOPE_VALUES.indexOf(scope);
        for (let i = 0; i < idx; i++) set.add(SCOPE_VALUES[i]);
      } else {
        set.delete(scope);
        // Deselecting a lower scope removes the higher ones that imply it.
        const idx = SCOPE_VALUES.indexOf(scope);
        for (let i = idx + 1; i < SCOPE_VALUES.length; i++)
          set.delete(SCOPE_VALUES[i]);
      }
      // Preserve canonical order.
      return SCOPE_VALUES.filter((s) => set.has(s));
    });
  };

  const submitCreate = async () => {
    const trimmed = name.trim();
    if (!trimmed || scopes.length === 0) return;
    try {
      const result = await createToken.mutateAsync({
        name: trimmed,
        scopes,
        // Only request create-site when the user has it AND toggled it on; the
        // server caps to users.can_create_sites regardless.
        canCreateSites: canCreateSites && grantCreateSites,
        // An empty expiry field means "no expiry" (null); otherwise send the
        // date as an ISO instant (datetime-local has no zone, so normalise).
        expiresAt: expiresAt ? new Date(expiresAt).toISOString() : null,
      });
      toast.success(t("created"));
      // Show-once: surface the plaintext exactly once in the dialog.
      setCreated(result);
      setName("");
      setScopes(["read", "write"]);
      setExpiresAt("");
      setGrantCreateSites(false);
    } catch (err) {
      toast.error(errorMessage(err, t("loadError")));
    }
  };

  const confirmRevoke = async () => {
    if (!pendingRevoke) return;
    try {
      await revokeToken.mutateAsync({ tokenId: pendingRevoke.id });
      toast.success(t("revoked"));
      setPendingRevoke(null);
    } catch (err) {
      toast.error(errorMessage(err, t("loadError")));
    }
  };

  return (
    <section
      data-slot="account-token-panel"
      className={cn("space-y-5", className)}
      aria-labelledby="tokens-heading"
    >
      <header className="space-y-1">
        <h2
          id="tokens-heading"
          className="flex items-center gap-2 text-xl font-semibold text-foreground"
        >
          <Sparkles className="size-5 text-primary" aria-hidden="true" />
          {t("title")}
        </h2>
        <p className="text-sm text-muted-foreground">{t("description")}</p>
      </header>

      {/* Issue form (any authenticated user — these are the user's OWN tokens) */}
      <form
        className="space-y-4 rounded-lg border border-border bg-card p-4"
        onSubmit={(e) => {
          e.preventDefault();
          void submitCreate();
        }}
      >
        <div className="grid gap-1.5">
          <Label htmlFor="token-name">{t("name")}</Label>
          <Input
            id="token-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t("namePlaceholder")}
            autoComplete="off"
          />
        </div>

        <fieldset className="space-y-2">
          <legend className="text-sm font-medium text-foreground">
            {t("scopes")}
          </legend>
          <div className="flex flex-wrap gap-4">
            {SCOPE_VALUES.map((scope) => {
              const id = `scope-${scope}`;
              return (
                <Label
                  key={scope}
                  htmlFor={id}
                  className="cursor-pointer gap-2 font-normal"
                >
                  <Checkbox
                    id={id}
                    checked={scopes.includes(scope)}
                    onCheckedChange={(checked) =>
                      toggleScope(scope, checked === true)
                    }
                  />
                  {t(`scope.${scope}`)}
                </Label>
              );
            })}
          </div>
        </fieldset>

        <div className="grid gap-1.5">
          <Label htmlFor="token-expiry">{t("expiresAt")}</Label>
          <Input
            id="token-expiry"
            type="datetime-local"
            value={expiresAt}
            onChange={(e) => setExpiresAt(e.target.value)}
            className="w-full sm:max-w-xs"
          />
          <p className="text-xs text-muted-foreground">{t("expiresHint")}</p>
        </div>

        {/* Create-site capability — only when the user themselves may create
            sites; otherwise hidden (the server caps it to false anyway). */}
        {canCreateSites ? (
          <div className="flex items-start justify-between gap-4 rounded-lg border border-border bg-background/40 p-3">
            <div className="space-y-0.5">
              <Label htmlFor="token-create-sites" className="font-medium">
                {t("canCreateSites")}
              </Label>
              <p className="text-xs text-muted-foreground">
                {t("canCreateSitesHint")}
              </p>
            </div>
            <Switch
              id="token-create-sites"
              checked={grantCreateSites}
              onCheckedChange={(checked) => setGrantCreateSites(checked)}
            />
          </div>
        ) : null}

        <div className="flex justify-end">
          <Button
            type="submit"
            disabled={
              createToken.isPending ||
              name.trim().length === 0 ||
              scopes.length === 0
            }
            aria-busy={createToken.isPending}
          >
            {createToken.isPending ? (
              <Spinner size="sm" />
            ) : (
              <KeyRound aria-hidden="true" />
            )}
            {t("issue")}
          </Button>
        </div>
      </form>

      {/* loading / error / empty / list */}
      {tokensQuery.isLoading ? (
        <LoadingState rows={2} label={t("title")} />
      ) : tokensQuery.isError ? (
        <ErrorState
          error={tokensQuery.error}
          title={t("loadError")}
          onRetry={() => tokensQuery.refetch()}
        />
      ) : tokens.length === 0 ? (
        <EmptyState
          icon={KeyRound}
          title={t("empty.title")}
          body={t("empty.body")}
        />
      ) : (
        <ul className="space-y-2">
          {tokens.map((tk) => (
            <li
              key={tk.id}
              className={cn(
                "flex flex-col gap-2 rounded-lg border border-border bg-card p-3 sm:flex-row sm:items-center sm:justify-between",
                tk.revokedAt && "opacity-60",
              )}
            >
              <div className="min-w-0 space-y-1">
                <p className="flex items-center gap-2 font-medium text-foreground">
                  <span className="truncate">{tk.name || tk.tokenPrefix}</span>
                  {tk.revokedAt ? (
                    <span className="text-xs text-destructive">
                      {t("revoked")}
                    </span>
                  ) : null}
                </p>
                <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                  <CodeText>{tk.tokenPrefix}…</CodeText>
                  {tk.scopes.map((s) => (
                    <Chip key={s}>{t(`scope.${s}`)}</Chip>
                  ))}
                  {tk.canCreateSites ? <Chip>{t("canCreateSites")}</Chip> : null}
                  {/* Last used (or "never used"). */}
                  <span>
                    {tk.lastUsedAt
                      ? t("lastUsed", {
                          when: format.dateTime(new Date(tk.lastUsedAt), {
                            dateStyle: "medium",
                          }),
                        })
                      : t("neverUsed")}
                  </span>
                  {/* Expiry (when set). */}
                  {tk.expiresAt ? (
                    <span>
                      {t("expiresOn", {
                        when: format.dateTime(new Date(tk.expiresAt), {
                          dateStyle: "medium",
                        }),
                      })}
                    </span>
                  ) : null}
                </div>
              </div>
              {!tk.revokedAt ? (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setPendingRevoke(tk)}
                  className="self-start sm:self-auto"
                >
                  {t("revoke")}
                </Button>
              ) : null}
            </li>
          ))}
        </ul>
      )}

      {/* Show-once dialog: the plaintext token + an MCP config snippet */}
      <Dialog
        open={created !== null}
        onOpenChange={(open) => {
          if (!open) setCreated(null);
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t("showOnceTitle")}</DialogTitle>
            <DialogDescription>{t("showOnceBody")}</DialogDescription>
          </DialogHeader>

          {created ? (
            <div className="space-y-4">
              {/* The secret — copy-once. CopyableUrl gives copy + truncation. */}
              <div className="space-y-1.5">
                <Label>{t("name")}</Label>
                <CopyableUrl value={created.token} />
              </div>

              {/* Example MCP client config */}
              <div className="space-y-1.5">
                <Label>MCP</Label>
                <pre className="max-h-60 overflow-auto rounded-lg border border-border bg-muted p-3 font-mono text-xs leading-relaxed text-foreground">
                  <code>{mcpConfigSnippet(created.token, endpoint)}</code>
                </pre>
                <CopyableUrl
                  value={mcpConfigSnippet(created.token, endpoint)}
                  label={tc("copy")}
                  className="justify-end"
                />
              </div>
            </div>
          ) : null}

          <DialogFooter>
            <Button onClick={() => setCreated(null)}>{tc("close")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Revoke confirm */}
      <ConfirmDialog
        open={pendingRevoke !== null}
        onOpenChange={(open) => {
          if (!open) setPendingRevoke(null);
        }}
        variant="destructive"
        title={t("revokeConfirmTitle")}
        description={t("revokeConfirmBody", {
          name: pendingRevoke?.name || pendingRevoke?.tokenPrefix || "",
        })}
        confirmLabel={t("revoke")}
        onConfirm={confirmRevoke}
        loading={revokeToken.isPending}
      />
    </section>
  );
}
