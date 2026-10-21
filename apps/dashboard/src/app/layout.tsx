import type { Metadata } from "next";
import { Inter } from "next/font/google";
import "./globals.css";
import { SessionProvider } from "@/components/session-provider";
import { ToastProvider } from "@/components/ui";
import { THEME_BOOTSTRAP } from "@/lib/theme";

const inter = Inter({ subsets: ["latin"], variable: "--font-sans", display: "swap" });

export const metadata: Metadata = {
  title: { default: "EdgePilot", template: "%s · EdgePilot" },
  description: "Fleet management for edge devices",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    // The theme script adds `dark` before hydration, so the server-rendered
    // class list legitimately differs from the client's.
    <html lang="en" className={inter.variable} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_BOOTSTRAP }} />
      </head>
      <body>
        <SessionProvider>
          <ToastProvider>{children}</ToastProvider>
        </SessionProvider>
      </body>
    </html>
  );
}
