import type { NextConfig } from "next";
import createNextIntlPlugin from "next-intl/plugin";

// Point the next-intl plugin at our request config (cookie-based locale,
// CANONICAL.md decision #5). This wires getRequestConfig into the server build.
const withNextIntl = createNextIntlPlugin("./src/i18n/request.ts");

const nextConfig: NextConfig = {
  // Emit a self-contained server bundle so the Docker runtime image carries only
  // the files Next needs (see deploy/frontend.Dockerfile).
  output: "standalone",
};

export default withNextIntl(nextConfig);
