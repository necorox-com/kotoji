/**
 * IconButton (atom) — design.md §3.1/§3.2. A Button in icon size that REQUIRES
 * an `aria-label` (icon-only buttons must be labeled, §4.8) and an optional
 * Tooltip. Touch hit-area is padded toward 44×44 (§2.4) via a hit-area span
 * while keeping the visual 32–36px control.
 */

import { Button } from "@/components/ui/button";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

type ButtonProps = React.ComponentProps<typeof Button>;

export interface IconButtonProps extends Omit<ButtonProps, "size"> {
  /** REQUIRED accessible name (this is the only label an icon button has). */
  "aria-label": string;
  /** Optional tooltip text; defaults to the aria-label for free affordance. */
  tooltip?: string;
  size?: "icon-xs" | "icon-sm" | "icon" | "icon-lg";
  /** Render the tooltip wrapper (default true when a label exists). */
  withTooltip?: boolean;
}

export function IconButton({
  tooltip,
  withTooltip = true,
  size = "icon",
  variant = "ghost",
  children,
  ...props
}: IconButtonProps) {
  const label = props["aria-label"];
  const button = (
    <Button type="button" variant={variant} size={size} {...props}>
      {children}
    </Button>
  );

  if (!withTooltip) return button;

  return (
    <Tooltip>
      <TooltipTrigger render={button} />
      <TooltipContent>{tooltip ?? label}</TooltipContent>
    </Tooltip>
  );
}
