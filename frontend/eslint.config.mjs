import { defineConfig, globalIgnores } from "eslint/config";
import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";
import jsxA11y from "eslint-plugin-jsx-a11y";

const eslintConfig = defineConfig([
  ...nextVitals,
  ...nextTs,
  // WCAG AA is enforced, not optional (design.md §4.8). eslint-config-next
  // already registers the jsx-a11y plugin, so we only layer its recommended
  // RULES on our JSX (re-registering the plugin would error). We exclude
  // src/components/ui/** because those are shadcn-GENERATED low-level primitives
  // (design.md §4.9: "do not hand-edit beyond shadcn"); a11y is asserted at the
  // atom/molecule/organism layer where these primitives are composed.
  {
    files: ["src/**/*.{ts,tsx}"],
    ignores: ["src/components/ui/**"],
    rules: jsxA11y.flatConfigs.recommended.rules,
  },
  // The generated openapi schema is machine-output; do not lint it.
  globalIgnores([
    // Default ignores of eslint-config-next:
    ".next/**",
    "out/**",
    "build/**",
    "next-env.d.ts",
    "src/lib/api/schema.d.ts",
  ]),
]);

export default eslintConfig;
