import type { Metadata } from "next";
import "./globals.css";
import { TokenProvider } from "../lib/auth";

export const metadata: Metadata = {
  title: "Skquad v2",
  description: "Skquad control plane — redesigned UI (A/B testing)",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <TokenProvider>{children}</TokenProvider>
      </body>
    </html>
  );
}
