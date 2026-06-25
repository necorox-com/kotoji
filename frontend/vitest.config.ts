/**
 * Vitest config for the frontend unit tests.
 *
 * jsdom gives us a DOM (document.cookie, File, etc.) so client hooks like
 * useUploadZip can be exercised exactly as they run in the browser. The `@`
 * alias mirrors tsconfig.json so test imports match app imports.
 */
import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

export default defineConfig({
  test: {
    environment: "jsdom",
    // Co-located *.test.ts(x) next to the code they cover.
    include: ["src/**/*.test.{ts,tsx}"],
    globals: false,
  },
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
});
