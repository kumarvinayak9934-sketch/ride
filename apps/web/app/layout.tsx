import type { Metadata } from "next";
import Link from "next/link";
import "./globals.css";
import { Nav } from "@/components/Nav";

export const metadata: Metadata = {
  title: "Safar Share — carpool together, split the cost",
  description:
    "India-first private-car cost-sharing carpool. Publish your corridor, find rides on yours, split fuel honestly.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body className="min-h-screen bg-slate-50 text-slate-900 antialiased">
        <header className="border-b border-slate-200 bg-white">
          <nav className="mx-auto flex max-w-5xl items-center justify-between px-4 py-3">
            <Link href="/" className="flex items-center gap-2 text-lg font-bold text-brand-700">
              <span aria-hidden>🚗</span> Safar Share
            </Link>
            <Nav />
          </nav>
        </header>
        <main className="mx-auto max-w-5xl px-4 py-8">{children}</main>
        <footer className="mx-auto max-w-5xl px-4 py-8 text-xs text-slate-400">
          Safar Share MVP — cost-sharing, not taxi. 18+ only. © {new Date().getFullYear()}
        </footer>
      </body>
    </html>
  );
}
