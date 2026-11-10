import type { Metadata, Viewport } from "next";
import { Inter, Geist_Mono } from "next/font/google";

import "./globals.css";
import "./theme.css";
import { Providers } from "@/components/providers";

// Inter with its optical-size axis: Fluid Functionalism animates a label's
// weight together with opsz, so it gets bolder without getting wider. Without
// the axis (or with a static Inter) weight changes shift the layout.
const inter = Inter({ subsets: ["latin"], axes: ["opsz"], variable: "--font-sans" });
const mono = Geist_Mono({ subsets: ["latin"], variable: "--font-mono" });

export const metadata: Metadata = {
  title: "Dorf",
  description: "Coding agents, each Thread in a Linux machine of its own.",
  appleWebApp: { capable: true, title: "Dorf", statusBarStyle: "default" },
};

export const viewport: Viewport = { width: "device-width", initialScale: 1, viewportFit: "cover" };

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html
      lang="en"
      suppressHydrationWarning
      className={`${inter.variable} ${mono.variable} font-sans antialiased`}
    >
      <body>
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
