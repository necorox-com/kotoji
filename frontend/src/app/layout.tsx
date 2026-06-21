import type { Metadata } from "next";
import { Inter, JetBrains_Mono, Noto_Sans_JP } from "next/font/google";
import { getLocale, getMessages } from "next-intl/server";
import "./globals.css";
import { Providers } from "@/components/providers";

// JP-capable font stack (design.md §2.3). Inter (latin UI) + Noto Sans JP
// (japanese), JetBrains Mono (code/SHA/URL). Noto Sans JP is multi-MB, so it is
// restricted to 3 weights with preload disabled — system-ui paints first.
const inter = Inter({
  variable: "--font-inter",
  subsets: ["latin"],
  display: "swap",
});

const notoSansJP = Noto_Sans_JP({
  variable: "--font-noto-sans-jp",
  weight: ["400", "500", "700"],
  preload: false,
  display: "swap",
});

const jetbrainsMono = JetBrains_Mono({
  variable: "--font-jbmono",
  subsets: ["latin"],
  display: "swap",
});

export const metadata: Metadata = {
  title: "kotoji",
  description: "あなたのツールに、住処を。",
};

export default async function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  // Resolve the active locale + its messages on the server (cookie-based, no URL
  // prefix; src/i18n/request.ts) so the client provider hydrates without a fetch
  // waterfall and <html lang> is correct (design.md §4.8 language landmark).
  const locale = await getLocale();
  const messages = await getMessages();

  // suppressHydrationWarning: next-themes sets the `class`/`style` on <html>
  // before React hydrates, which would otherwise trip a mismatch warning.
  return (
    <html
      lang={locale}
      suppressHydrationWarning
      className={`${inter.variable} ${notoSansJP.variable} ${jetbrainsMono.variable} h-full`}
    >
      <body className="min-h-full">
        <Providers locale={locale} messages={messages}>
          {children}
        </Providers>
      </body>
    </html>
  );
}
