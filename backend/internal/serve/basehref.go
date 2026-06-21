package serve

import (
	"bytes"

	"github.com/necorox-com/kotoji/backend/internal/resolve"
)

// baseHrefScanLimit bounds how far we scan for the <head> open tag before giving
// up and serving the document unmodified (routing-and-serving.md §6.4 impl note:
// a bounded byte scan, never a full HTML parse on the hot path).
const baseHrefScanLimit = 64 * 1024 // 64 KB

// ServeOptions toggles path-mode body transforms. Host mode ignores all of these
// (each project is its own origin root; no injection needed).
type ServeOptions struct {
	// InjectBaseHref is effective ONLY when Source==SourcePath && content is HTML.
	// Default true in path mode (set by the handler from CacheConfig).
	InjectBaseHref bool
}

// maybeInjectBaseHref returns the (possibly transformed) HTML body and whether it
// was modified, per routing-and-serving.md §6.4.
//
// Hard limits (documented loudly in the contract and honored here):
//   - Only effective in PATH mode (t.Source == SourcePath); Host mode is unaffected.
//   - Only fixes RELATIVE URLs and anchor resolution. It does NOT rewrite
//     root-absolute URLs (/style.css stays /style.css), JS-constructed URLs, or
//     CSS url(/...). Host mode is the supported mode for root-absolute tools.
//   - Skipped if a <base> already exists (never inject two).
//   - Skipped if <head> is not found within baseHrefScanLimit bytes.
//
// The injected tag is "<base href="{PathPrefix}/">", placed immediately after the
// <head ...> open tag, or after a leading <meta charset> if one is the first child
// (to not disturb charset detection).
func maybeInjectBaseHref(body []byte, t resolve.Target, opts ServeOptions) ([]byte, bool) {
	if t.Source != resolve.SourcePath || !opts.InjectBaseHref || t.PathPrefix == "" {
		return body, false
	}

	// Bound the scan region.
	scan := body
	if len(scan) > baseHrefScanLimit {
		scan = scan[:baseHrefScanLimit]
	}
	lower := bytes.ToLower(scan)

	// If a <base ...> already exists anywhere in the scanned head region, do not
	// inject a second one.
	if i := bytes.Index(lower, []byte("<base")); i >= 0 {
		// Ensure it's a tag boundary ("<base " or "<base>"), not "<basefoo".
		after := i + len("<base")
		if after >= len(lower) || lower[after] == ' ' || lower[after] == '>' || lower[after] == '\t' || lower[after] == '\n' || lower[after] == '\r' || lower[after] == '/' {
			return body, false
		}
	}

	headIdx := bytes.Index(lower, []byte("<head"))
	if headIdx < 0 {
		return body, false // no <head> within the scan window -> serve unmodified
	}
	// Find the end '>' of the <head ...> open tag.
	gt := bytes.IndexByte(lower[headIdx:], '>')
	if gt < 0 {
		return body, false // truncated head tag in the window -> bail
	}
	insertAt := headIdx + gt + 1

	// If the immediate next element is <meta charset=...>, insert AFTER it so we do
	// not move the charset declaration past the first 1024 bytes (charset sniffing).
	if metaEnd, ok := metaCharsetEnd(lower, insertAt); ok {
		insertAt = metaEnd
	}

	tag := []byte(`<base href="` + t.PathPrefix + `/">`)
	out := make([]byte, 0, len(body)+len(tag))
	out = append(out, body[:insertAt]...)
	out = append(out, tag...)
	out = append(out, body[insertAt:]...)
	return out, true
}

// metaCharsetEnd reports the index just past a <meta charset ...> tag if one
// begins (ignoring whitespace) at or right after `from` in the lowercased body.
// It allows a run of whitespace before the meta tag.
func metaCharsetEnd(lower []byte, from int) (int, bool) {
	i := from
	for i < len(lower) && isASCIISpace(lower[i]) {
		i++
	}
	if !bytes.HasPrefix(lower[i:], []byte("<meta")) {
		return 0, false
	}
	gt := bytes.IndexByte(lower[i:], '>')
	if gt < 0 {
		return 0, false
	}
	metaTag := lower[i : i+gt+1]
	if !bytes.Contains(metaTag, []byte("charset")) {
		return 0, false
	}
	return i + gt + 1, true
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}
