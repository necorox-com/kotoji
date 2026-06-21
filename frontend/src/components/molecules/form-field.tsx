"use client";

/**
 * FormField (molecule) — design.md §3.2. The single way forms render a field:
 * Label (+required marker) + control + help text + error, all a11y-wired via the
 * ui/form RHF primitives (aria-describedby/aria-invalid, §4.8). Pass the control
 * as children via a render prop so the field works with Input, Textarea, Select,
 * etc. uniformly.
 */

import { useTranslations } from "next-intl";
import {
  type Control,
  type FieldPath,
  type FieldValues,
  type ControllerRenderProps,
} from "react-hook-form";
import {
  FormControl,
  FormDescription,
  FormField as RHFFormField,
  FormItem,
  FormLabel,
  FormMessage,
} from "@/components/ui/form";

export interface FormFieldProps<
  TFieldValues extends FieldValues = FieldValues,
  TName extends FieldPath<TFieldValues> = FieldPath<TFieldValues>,
> {
  control: Control<TFieldValues>;
  name: TName;
  label?: React.ReactNode;
  /** Mark the label required (shows * + sr-only "必須"). */
  required?: boolean;
  /** Help text shown below the control. */
  description?: React.ReactNode;
  /** Render the control; receives the RHF field bindings (value/onChange/...). */
  render: (
    field: ControllerRenderProps<TFieldValues, TName>
  ) => React.ReactElement;
}

export function FormField<
  TFieldValues extends FieldValues = FieldValues,
  TName extends FieldPath<TFieldValues> = FieldPath<TFieldValues>,
>({
  control,
  name,
  label,
  required,
  description,
  render,
}: FormFieldProps<TFieldValues, TName>) {
  const t = useTranslations("common");
  return (
    <RHFFormField
      control={control}
      name={name}
      render={({ field }) => (
        <FormItem>
          {label ? (
            <FormLabel>
              {label}
              {required ? (
                <>
                  <span aria-hidden="true" className="text-destructive">
                    {" *"}
                  </span>
                  <span className="sr-only">{` (${t("required")})`}</span>
                </>
              ) : null}
            </FormLabel>
          ) : null}
          <FormControl>{render(field)}</FormControl>
          {description ? (
            <FormDescription>{description}</FormDescription>
          ) : null}
          <FormMessage />
        </FormItem>
      )}
    />
  );
}
