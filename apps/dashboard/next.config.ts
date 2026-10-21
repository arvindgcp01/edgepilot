import type { NextConfig } from "next";

const config: NextConfig = {
  reactStrictMode: true,
  // The dashboard is a pure API client: it holds no server-side secrets and
  // renders nothing on the server that the browser could not fetch itself.
  // Standalone output keeps the container to the ~40 MB the runtime needs.
  output: "standalone",
  poweredByHeader: false,

  /**
   * Proxy the API through this origin.
   *
   * # Why this is not optional
   *
   * The refresh token is an `HttpOnly; Secure; SameSite=Strict` cookie. Those
   * three attributes are the reason an XSS cannot steal a long-lived
   * credential — and they also mean the browser will not send the cookie to a
   * *different* site, and will not store it at all over plain HTTP.
   *
   * A dashboard on localhost:3000 calling an API on another host is cross-site
   * by definition. The cookie would be dropped, and the session would vanish on
   * the first reload. Weakening the cookie to `SameSite=Lax` would fix the
   * symptom by removing the protection.
   *
   * So the dashboard and the API share an origin. Here that is a rewrite; in
   * production it is one TLS terminator in front of both, which is what
   * docs/Deployment.md §4 describes. Same shape, different mechanism.
   */
  async rewrites() {
    const upstream = process.env.EP_API_UPSTREAM;
    if (!upstream) return [];
    return [{ source: "/api/:path*", destination: `${upstream.replace(/\/$/, "")}/api/:path*` }];
  },
};

export default config;
