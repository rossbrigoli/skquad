import type { Metadata } from "next";
import "./globals.css";
import { TokenProvider } from "../lib/auth";
import { AttentionProvider } from "../lib/useAttention";

export const metadata: Metadata = {
  title: "Skquad v2",
  description: "Skquad control plane — redesigned UI (A/B testing)",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <TokenProvider>
          <AttentionProvider>{children}</AttentionProvider>
        </TokenProvider>
      </body>
    </html>
  );
}
