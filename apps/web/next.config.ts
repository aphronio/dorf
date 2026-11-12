import type { NextConfig } from "next"

// The agent serves the static export; in development, API calls go to the agent named by DORF_AGENT.
const agent = process.env.DORF_AGENT

const nextConfig: NextConfig =
  process.env.NODE_ENV === "production"
    ? { output: "export", images: { unoptimized: true } }
    : {
        // Compression would hold back the Threads' event streams.
        compress: false,
        devIndicators: false,
        allowedDevOrigins: process.env.DORF_DEV_ORIGINS?.split(","),
        async rewrites() {
          return agent
            ? ["threads", "repos", "claude", "github", "models"].flatMap((root) => [
                { source: `/${root}`, destination: `${agent}/${root}` },
                { source: `/${root}/:path*`, destination: `${agent}/${root}/:path*` },
              ])
            : []
        },
      }

export default nextConfig
