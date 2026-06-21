/** Generic UI hooks barrel (design.md §4.9 src/hooks/). */

export { useMediaQuery, useBreakpoint } from "./use-media-query";
export { useCopyToClipboard, type UseCopyResult } from "./use-copy-to-clipboard";
export { useDebounce } from "./use-debounce";
export {
  useRequireAuth,
  type UseRequireAuthOptions,
} from "./use-require-auth";
export { useSiteRole, type UseSiteRoleResult } from "./use-site-role";
