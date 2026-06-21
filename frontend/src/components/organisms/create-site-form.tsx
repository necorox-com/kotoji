"use client";

/**
 * CreateSiteForm (organism) — design.md §3.3 / §3.5 CreateSite.
 *
 * Three-step single-column form (max-w-2xl on the page):
 *  ① start mode: empty / zip / template (accessible radio cards — there is no
 *     radio-group ui/ primitive in this registry, so native <input type=radio>
 *     inside styled labels gives correct keyboard + screen-reader semantics),
 *  ② handle (live-validated per CANONICAL §5) + live URL preview,
 *  ③ visibility + description.
 *
 * Validation (react-hook-form + zod, design §4.1):
 *  - SYNC handle rules (CANONICAL §5.1): ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$,
 *    no "--" substring, min/max length from useConfig (fallback 3/63),
 *    not a reserved handle (from useConfig.reservedHandles).
 *  - ASYNC uniqueness is the SERVER's job: a handle_taken 409 on submit is shown
 *    inline on the handle field (we don't duplicate the reservation race here).
 *
 * On success → toast + onCreated(handle) (the page redirects to ProjectDetail).
 * The zip seed (when mode='zip') is uploaded AFTER create via useUploadZip onto
 * the new site's draft branch (initial seed: no baseSha).
 */

import { useMemo, useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { z } from "zod";
import {
  FileArchive,
  FilePlus2,
  LayoutTemplate,
  UploadCloud,
} from "lucide-react";
import { useTranslations } from "next-intl";
import { toast } from "sonner";

import { FormField } from "@/components/molecules/form-field";
import { CodeText } from "@/components/atoms/code-text";
import { Spinner } from "@/components/atoms";
import { Form } from "@/components/ui/form";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useConfig, useCreateSite } from "@/lib/api/hooks";
import { isApiError, errorMessage } from "@/lib/api/error";
import type { SiteVisibility } from "@/lib/api/types";
import { cn } from "@/lib/utils";

// Fallbacks if useConfig hasn't resolved yet (CANONICAL §5.1 defaults).
const DEFAULT_MIN = 3;
const DEFAULT_MAX = 63;
const DEFAULT_BASE_DOMAIN = "hosting.example.com";
// CANONICAL §5.1 baseline reserved blocklist (the Go constant fallback).
const FALLBACK_RESERVED = [
  "draft",
  "preview",
  "published",
  "www",
  "api",
  "internal",
  "host",
  "admin",
  "app",
  "static",
  "assets",
  "mcp",
];

// DNS-label grammar (CANONICAL §5.1). The no-"--" rule is a separate check.
const HANDLE_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

type StartMode = "empty" | "zip" | "template";
const VISIBILITY_VALUES: SiteVisibility[] = ["private", "internal", "public"];

export interface CreateSiteFormProps {
  /**
   * Called after a successful create. The page redirects to ProjectDetail and,
   * when a seed zip was chosen (mode='zip'), uploads it onto the new site's
   * draft branch there via useUploadZip (the upload hook binds a handle at
   * creation, so the seed is performed by the page that knows the new handle —
   * keeping this form free of cross-handle upload plumbing; CANONICAL §1
   * ImportZip is a post-create op anyway).
   */
  onCreated?: (handle: string, seedZip?: File | null) => void;
  /** Cancel handler (page navigates back). */
  onCancel?: () => void;
  className?: string;
}

export function CreateSiteForm({
  onCreated,
  onCancel,
  className,
}: CreateSiteFormProps) {
  const t = useTranslations("createSite");
  const tc = useTranslations("common");
  const tSettings = useTranslations("settings");

  const configQuery = useConfig();
  const createSite = useCreateSite();

  const config = configQuery.data;
  const minLen = config?.handleMinLen ?? DEFAULT_MIN;
  const maxLen = config?.handleMaxLen ?? DEFAULT_MAX;
  const baseDomain = config?.baseDomain ?? DEFAULT_BASE_DOMAIN;
  const reserved = config?.reservedHandles ?? FALLBACK_RESERVED;

  const [mode, setMode] = useState<StartMode>("empty");
  const [zipFile, setZipFile] = useState<File | null>(null);

  // Build the zod schema from config so messages + bounds stay in sync. The
  // handle field carries every CANONICAL §5.1 rule with localized messages.
  const schema = useMemo(
    () =>
      z.object({
        handle: z
          .string()
          .min(minLen, { message: t("handleTooShort", { min: minLen }) })
          .max(maxLen, { message: t("handleTooLong", { max: maxLen }) })
          .regex(HANDLE_RE, { message: t("handleInvalid") })
          .refine((h) => !h.includes("--"), { message: t("handleInvalid") })
          .refine((h) => !reserved.includes(h.toLowerCase()), {
            message: t("handleReserved"),
          }),
        visibility: z.enum(["public", "internal", "private"]),
        description: z.string().max(280).optional(),
      }),
    [minLen, maxLen, reserved, t],
  );

  type FormValues = z.infer<typeof schema>;

  const form = useForm<FormValues>({
    resolver: zodResolver(schema),
    mode: "onChange",
    defaultValues: { handle: "", visibility: "private", description: "" },
  });

  // Live URL preview from the current handle value. We use `useWatch` (the
  // subscription hook) rather than `form.watch()` so the value is React
  // Compiler-safe (the returned `watch` function can't be memoized and triggers
  // the react-hooks/incompatible-library rule).
  const handleValue = useWatch({ control: form.control, name: "handle" }) ?? "";
  const previewHost =
    handleValue && HANDLE_RE.test(handleValue) && !handleValue.includes("--")
      ? `${handleValue}.${baseDomain}`
      : `…${"." + baseDomain}`;

  const handleState = form.getFieldState("handle", form.formState);
  const handleValid =
    handleValue.length >= minLen && !handleState.invalid && handleState.isDirty;

  const onSubmit = form.handleSubmit(async (values) => {
    try {
      const site = await createSite.mutateAsync({
        handle: values.handle,
        visibility: values.visibility,
        publishMode: config?.defaultPublishMode ?? "direct",
        description: values.description ?? "",
      });

      toast.success(t("created"));
      // Hand the new handle (and any seed zip) up; the page redirects and, for
      // mode='zip', uploads the seed onto the new site's draft branch.
      onCreated?.(site.handle, mode === "zip" ? zipFile : null);
    } catch (err) {
      // Map a server handle_taken 409 onto the handle field inline; otherwise
      // surface a toast (network/internal).
      if (isApiError(err) && err.code === "handle_taken") {
        form.setError("handle", { message: t("handleTaken") });
      } else {
        toast.error(errorMessage(err, t("createError")));
      }
    }
  });

  return (
    <Form {...form}>
      <form
        onSubmit={onSubmit}
        className={cn("space-y-8", className)}
        data-slot="create-site-form"
        noValidate
      >
        {/* ① Start mode */}
        <fieldset className="space-y-3">
          <legend className="text-sm font-medium text-foreground">
            {t("stepStart")}
          </legend>
          <div
            role="radiogroup"
            aria-label={t("stepStart")}
            className="grid gap-3 sm:grid-cols-3"
          >
            <ModeCard
              value="empty"
              current={mode}
              onSelect={setMode}
              icon={FilePlus2}
              label={t("startEmpty")}
            />
            <ModeCard
              value="zip"
              current={mode}
              onSelect={setMode}
              icon={FileArchive}
              label={t("startZip")}
            />
            <ModeCard
              value="template"
              current={mode}
              onSelect={setMode}
              icon={LayoutTemplate}
              label={t("startTemplate")}
              disabled
            />
          </div>
        </fieldset>

        {/* ② Handle + live URL preview */}
        <div className="space-y-2">
          <FormField
            control={form.control}
            name="handle"
            label={t("handleLabel")}
            required
            description={t("handleHelp")}
            render={(field) => (
              <Input
                {...field}
                value={field.value ?? ""}
                onChange={(e) =>
                  // Normalize to lowercase as the user types (handles are
                  // lowercased before store — CANONICAL §5.1).
                  field.onChange(e.target.value.toLowerCase())
                }
                placeholder="expense-calc"
                className="font-mono"
                autoComplete="off"
                inputMode="text"
                aria-describedby="handle-url-preview"
              />
            )}
          />
          <p id="handle-url-preview" className="text-sm text-muted-foreground">
            {t("urlPreview")}: <CodeText>{`https://${previewHost}`}</CodeText>
            {handleValid ? (
              <span className="ml-2 text-success">
                ✓ {t("handleAvailable")}
              </span>
            ) : null}
          </p>
        </div>

        {/* ③ (zip mode) file picker */}
        {mode === "zip" ? (
          <div className="space-y-2">
            <label
              htmlFor="create-zip"
              className={cn(
                "flex cursor-pointer flex-col items-center justify-center gap-2 rounded-xl border border-dashed border-border bg-muted/30 px-6 py-10 text-center transition-colors hover:border-ring",
                zipFile && "border-success/50 bg-success/5",
              )}
            >
              <UploadCloud
                className="size-6 text-muted-foreground"
                aria-hidden="true"
              />
              <span className="text-sm text-muted-foreground">
                {zipFile ? zipFile.name : t("startZip")}
              </span>
              <input
                id="create-zip"
                type="file"
                accept=".zip,application/zip"
                className="sr-only"
                onChange={(e) => setZipFile(e.target.files?.[0] ?? null)}
              />
            </label>
          </div>
        ) : null}

        {/* ④ Visibility */}
        <FormField
          control={form.control}
          name="visibility"
          label={tSettings("visibility")}
          render={(field) => (
            <Select
              value={field.value}
              onValueChange={(v) => v != null && field.onChange(v)}
            >
              <SelectTrigger
                className="w-full"
                aria-label={tSettings("visibility")}
              >
                <SelectValue>
                  {(v: SiteVisibility) =>
                    tSettings(
                      v === "public"
                        ? "visibilityPublic"
                        : v === "internal"
                          ? "visibilityInternal"
                          : "visibilityPrivate",
                    )
                  }
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                {VISIBILITY_VALUES.map((v) => (
                  <SelectItem key={v} value={v}>
                    {tSettings(
                      v === "public"
                        ? "visibilityPublic"
                        : v === "internal"
                          ? "visibilityInternal"
                          : "visibilityPrivate",
                    )}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
        />

        {/* ⑤ Description */}
        <FormField
          control={form.control}
          name="description"
          label={t("stepDescription")}
          render={(field) => (
            <Textarea
              {...field}
              value={field.value ?? ""}
              placeholder={t("descriptionPlaceholder")}
            />
          )}
        />

        {/* Actions */}
        <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
          {onCancel ? (
            <Button
              type="button"
              variant="outline"
              onClick={onCancel}
              disabled={createSite.isPending}
            >
              {tc("cancel")}
            </Button>
          ) : null}
          <Button
            type="submit"
            disabled={
              createSite.isPending ||
              !form.formState.isValid ||
              (mode === "zip" && !zipFile)
            }
            aria-busy={createSite.isPending}
          >
            {createSite.isPending ? <Spinner size="sm" /> : null}
            {t("submit")}
          </Button>
        </div>
      </form>
    </Form>
  );
}

/** One selectable start-mode card backed by a native radio for a11y. */
function ModeCard({
  value,
  current,
  onSelect,
  icon: Icon,
  label,
  disabled,
}: {
  value: StartMode;
  current: StartMode;
  onSelect: (v: StartMode) => void;
  icon: typeof FilePlus2;
  label: string;
  disabled?: boolean;
}) {
  const selected = current === value;
  return (
    <label
      className={cn(
        "flex cursor-pointer flex-col items-center gap-2 rounded-lg border p-4 text-center text-sm transition-colors",
        selected
          ? "border-primary bg-accent text-accent-foreground ring-1 ring-primary"
          : "border-border hover:bg-muted",
        disabled && "pointer-events-none opacity-50",
      )}
    >
      <input
        type="radio"
        name="start-mode"
        value={value}
        checked={selected}
        disabled={disabled}
        onChange={() => onSelect(value)}
        className="sr-only"
      />
      <Icon className="size-5 text-muted-foreground" aria-hidden="true" />
      <span className="font-medium">{label}</span>
    </label>
  );
}
