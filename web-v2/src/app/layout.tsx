import type { Metadata } from "next";
import "./globals.css";
import { TokenProvider } from "../lib/auth";
import { AttentionProvider } from "../lib/useAttention";
import { ThemeProvider } from "../lib/ThemeProvider";
import { THEME_INIT_SCRIPT } from "../lib/theme";

export const metadata: Metadata = {
  title: "Skquad v2",
  description: "Skquad control plane — redesigned UI (A/B testing)",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        {/* Pre-paint theme application (S-101): prevents a flash of the
            wrong theme before React hydrates. Keep in sync with lib/theme.ts. */}
        <script dangerouslySetInnerHTML={{ __html: THEME_INIT_SCRIPT }} />
      </head>
      <body>
        <ThemeProvider>
          <TokenProvider>
            <AttentionProvider>{children}</AttentionProvider>
          </TokenProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}
