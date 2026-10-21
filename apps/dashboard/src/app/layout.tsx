import type { Metadata } from "next";
import "./globals.css";
import { SessionProvider } from "@/components/session-provider";
import { ToastProvider } from "@/components/ui";

export const metadata: Metadata = {
  title: "EdgePilot",
  description: "Fleet management for macOS devices",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <SessionProvider>
          <ToastProvider>{children}</ToastProvider>
        </SessionProvider>
      </body>
    </html>
  );
}
