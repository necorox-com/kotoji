package site

import (
	"path"
	"path/filepath"
	"strings"
)

// gitDirSegment is the repo metadata directory that must never be writable or
// servable, even if a path somehow contains it.
const gitDirSegment = ".git"

// validatePath is the repo-relative file-path gate used by every read/write
// (CANONICAL §5.4). It rejects: empty paths (for files), leading "/", absolute
// paths, any ".." element, backslashes, NUL bytes, and the ".git" segment; it
// uses filepath.IsLocal + path.Clean and re-checks that no ".." remains. The git
// layer additionally operates via the index (blob writes), never os.Open of a
// joined user path — so this is the friendly primary gate, not the only defense.
//
// allowExt gates the extension allowlist: callers that write content (WriteFile)
// pass the upload allowlist check; pure reads pass a no-extension-check variant
// via validateReadPath. Returns a *ValidationError (Is ErrValidation).
func validatePath(p string) error {
	return validatePathWith(p, extAllowedForUpload)
}

// validateMCPWritePath is the stricter variant for MCP writes: text-first
// allowlist (excludes large media). Used by callers that originate from an MCP
// Actor.Via == SourceMCP write.
func validateMCPWritePath(p string) error {
	return validatePathWith(p, extAllowedForMCPWrite)
}

// validateReadPath validates a path for reads: same traversal/structure rules
// but NO extension allowlist (you may read any committed file, including ones
// uploaded before an allowlist change).
func validateReadPath(p string) error {
	return validatePathWith(p, nil)
}

// validatePathWith is the shared core. extOK, when non-nil, is the extension
// allowlist predicate applied after the structural checks pass.
func validatePathWith(p string, extOK func(string) bool) error {
	if p == "" {
		return &ValidationError{Field: "path", Reason: "must not be empty"}
	}
	if strings.ContainsRune(p, 0x00) {
		return &ValidationError{Field: "path", Reason: "must not contain NUL bytes"}
	}
	if strings.Contains(p, `\`) {
		return &ValidationError{Field: "path", Reason: "must use forward slashes"}
	}
	if strings.HasPrefix(p, "/") {
		return &ValidationError{Field: "path", Reason: "must be repo-relative (no leading slash)"}
	}
	// filepath.IsLocal rejects absolute paths and any path that escapes the
	// current root via "..", on the host OS's separator semantics. We already
	// rejected backslashes so this is robust on Windows builds too.
	if !filepath.IsLocal(p) {
		return &ValidationError{Field: "path", Reason: "must not escape the repo root"}
	}
	// Defense in depth: Clean a rooted form and confirm no ".." element survives
	// and that the path did not normalize to "." (which would mean "the root").
	clean := path.Clean(p)
	if clean == "." || clean == "" {
		return &ValidationError{Field: "path", Reason: "must reference a file, not the root"}
	}
	for _, seg := range strings.Split(clean, "/") {
		switch seg {
		case "..":
			return &ValidationError{Field: "path", Reason: `must not contain ".." segments`}
		case gitDirSegment:
			return &ValidationError{Field: "path", Reason: `must not reference the ".git" directory`}
		}
		// P1: reject any segment beginning with a dash. filepath.IsLocal is
		// dash-blind, so a path like "-foo" or "dir/--output=x" passes every check
		// above yet, if it reached an ls-tree / add sink as a positional, git would
		// parse it as an OPTION (argument injection). We reject it here independent of
		// any call-site "--" discipline so the validator alone closes the surface.
		if strings.HasPrefix(seg, "-") {
			return &ValidationError{Field: "path", Reason: "path segments must not begin with a dash"}
		}
	}
	if extOK != nil && !extOK(clean) {
		return &ValidationError{Field: "path", Reason: "file extension is not allowed"}
	}
	return nil
}

// validateDir validates a directory filter for listings: "" means root (always
// valid). Otherwise it must pass the same traversal rules as a file path but
// WITHOUT an extension check (directories have no extension).
func validateDir(dir string) error {
	if dir == "" {
		return nil
	}
	return validatePathWith(dir, nil)
}

// isBinaryContent applies the NUL-byte heuristic over a bounded prefix to flag
// binary content (FileContent.IsBinary, CANONICAL §2). A NUL byte in the first
// sniffLen bytes marks the file binary; otherwise extension is the tiebreaker.
func isBinaryContent(content []byte) bool {
	const sniffLen = 8000
	n := len(content)
	if n > sniffLen {
		n = sniffLen
	}
	for i := 0; i < n; i++ {
		if content[i] == 0x00 {
			return true
		}
	}
	return false
}
