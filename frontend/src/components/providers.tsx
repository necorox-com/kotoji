"use client";

/**
 * Providers — the client-side app shell wired once at the root layout.
 * Composes (design.md §4.2 / §4.4 / §4.7):
 *  - NextIntlClientProvider (ja default + en; CANONICAL.md decision #5),
 *  - next-themes (light/dark/system),
 *  - TanStack Query (server-state cache),
 *  - the shadcn TooltipProvider (base-ui under the hood; shared hover delay),
 *  - the theme-synced sonner Toaster (success/error/copy/publish announcements).
 *
 * Locale + messages are resolved on the server (layout.tsx) and passed in so the
 * provider is hydration-safe and there is no message-fetch waterfall.
 */

import { useState, type ReactNode } from "react";
import { ThemeProvider } from "next-themes";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NextIntlClientProvider, type AbstractIntlMessages } from "next-intl";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Toaster } from "@/components/ui/sonner";

// Default theme is configurable via NEXT_PUBLIC_DEFAULT_THEME (design.md §4.4).
const defaultTheme = process.env.NEXT_PUBLIC_DEFAULT_THEME ?? "system";

// Shared tooltip timing (design.md §2.10 motion language).
const TOOLTIP_DELAY_MS = 400;

/**
 * makeQueryClient builds a QueryClient with kotoji-sane defaults: a short stale
 * window and no refetch-on-focus storm (the editor/dashboard drive their own
 * invalidations). Created per-app-instance so server and client never share.
 */
function makeQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 30_000,
        refetchOnWindowFocus: false,
        retry: 1,
      },
    },
  });
}

export interface ProvidersProps {
  children: ReactNode;
  locale: string;
  messages: AbstractIntlMessages;
}

export function Providers({ children, locale, messages }: ProvidersProps) {
  // useState ensures one stable QueryClient for the component's lifetime
  // (avoids recreating the cache on every render).
  const [queryClient] = useState(makeQueryClient);

  return (
    <NextIntlClientProvider locale={locale} messages={messages}>
      <ThemeProvider
        attribute="class"
        defaultTheme={defaultTheme}
        enableSystem
        disableTransitionOnChange
      >
        <QueryClientProvider client={queryClient}>
          <TooltipProvider delay={TOOLTIP_DELAY_MS}>
            {children}
          </TooltipProvider>
          <Toaster richColors closeButton position="bottom-right" />
        </QueryClientProvider>
      </ThemeProvider>
    </NextIntlClientProvider>
  );
}
