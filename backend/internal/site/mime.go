package site

import (
	"path"
	"strings"
)

// MIMEByExt is the SINGLE SOURCE OF TRUTH for the served-content MIME table and,
// by extension, the upload allowlist (CANONICAL §5.6; routing-and-serving.md
// §5.3). Keeping them one table prevents an extension being uploadable but
// unserveable, or vice versa.
//
//   - Upload allowlist = keys(MIMEByExt).
//   - MCP write_file allowlist = keys minus large-media types (see MCPWriteDenied).
//
// Keys are lowercased, dot-prefixed extensions.
var MIMEByExt = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".txt":         "text/plain; charset=utf-8",
	".xml":         "application/xml; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".manifest":    "application/manifest+json",
	".pdf":         "application/pdf",
	".wasm":        "application/wasm",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
	".mp3":         "audio/mpeg",
	".wav":         "audio/wav",
	".csv":         "text/csv; charset=utf-8",
}

// mcpWriteDenied is the set of large-media extensions excluded from the MCP
// write_file allowlist — MCP is text-first; binaries are upload-only
// (CANONICAL §5.6, decision-adjacent consistency-report #P1-7).
var mcpWriteDenied = map[string]struct{}{
	".mp4":  {},
	".webm": {},
	".mp3":  {},
	".wav":  {},
	".pdf":  {},
}

// ContentTypeFor returns the deterministic Content-Type for a path's extension,
// or "" when the extension is not in the allowlist. The serve layer treats ""
// as octet-stream + attachment (fail-closed); the upload/write layers treat ""
// as a disallowed type.
func ContentTypeFor(p string) string {
	return MIMEByExt[strings.ToLower(path.Ext(p))]
}

// extAllowedForUpload reports whether the path's extension is in the upload/zip
// allowlist (= keys of MIMEByExt). Directories (no extension) are handled by the
// caller; this is a pure extension check.
func extAllowedForUpload(p string) bool {
	_, ok := MIMEByExt[strings.ToLower(path.Ext(p))]
	return ok
}

// extAllowedForMCPWrite reports whether the path's extension is writable via MCP:
// in the upload allowlist AND not a large-media type.
func extAllowedForMCPWrite(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	if _, ok := MIMEByExt[ext]; !ok {
		return false
	}
	_, denied := mcpWriteDenied[ext]
	return !denied
}
