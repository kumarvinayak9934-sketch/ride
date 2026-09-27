/** @type {import('next').NextConfig} */

// The browser only ever talks to same-origin /api/* (NEXT_PUBLIC_API_BASE);
// the dev/prod server proxies to the API container — the browser never needs
// to know the backend host (sandbox + CSP friendly).
const API_BASE = process.env.API_BASE ?? "http://localhost:8080";

const nextConfig = {
  reactStrictMode: true,
  async rewrites() {
    return [
      {
        source: "/api/:path*",
        destination: `${API_BASE}/:path*`,
      },
    ];
  },
};

export default nextConfig;
