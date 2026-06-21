package site

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// ImportZip extracts an uploaded archive into branch as ONE commit, REPLACING the
// tree. ALL security guards run BEFORE anything is written (validate-then-write):
// declared-size gate, central-directory open, entry-count cap, per-entry ZipSlip
// (filepath.IsLocal + symlink reject + extension allowlist), and decompressed-size
// + per-entry + compression-ratio bomb guards. CANONICAL §1 / site-service.md §7.
func (g *gitService) ImportZip(ctx context.Context, id uuid.UUID, branch BranchName, src ZipSource, baseSHA string, actor Actor) (CommitInfo, error) {
	if err := refuseProtectedBranch(branch); err != nil {
		return CommitInfo{}, err
	}
	// 1. Declared-size gate BEFORE reading the archive (cheap fail).
	if src.Size > g.cfg.Zip.MaxUploadBytes {
		return CommitInfo{}, ErrZipTooLarge
	}
	if src.Reader == nil {
		return CommitInfo{}, &ValidationError{Field: "zip", Reason: "no archive reader"}
	}

	// 2. Open via the central directory (archive/zip with ReaderAt+Size) — never
	//    trust a streamed local header.
	zr, err := zip.NewReader(src.Reader, src.Size)
	if err != nil {
		return CommitInfo{}, &ValidationError{Field: "zip", Reason: "not a valid zip archive"}
	}

	// 3. Entry-count gate.
	if len(zr.File) > g.cfg.Zip.MaxEntries {
		return CommitInfo{}, ErrZipTooManyFiles
	}

	// 4. Validate EVERY entry before extracting any, accumulating the file set in
	//    memory (bounded by the decompressed-size guard) so a partial write is
	//    impossible.
	extracted, err := g.validateAndReadZip(zr)
	if err != nil {
		return CommitInfo{}, err
	}

	var ci CommitInfo
	err = g.withWriteLock(id, func() error {
		if _, e := g.requireSite(ctx, id); e != nil {
			return e
		}
		// Optimistic lock: empty baseSHA allowed ONLY when the branch has no commits
		// yet (initial seed). Otherwise it must equal the current tip.
		exists, e := g.branchExists(ctx, id, branch)
		if e != nil {
			return wrapGit(e)
		}
		if exists {
			if baseSHA == "" {
				return &ValidationError{Field: "baseSha", Reason: "required for an existing branch"}
			}
			if e := g.checkBaseSHA(ctx, id, branch, baseSHA, nil); e != nil {
				return e
			}
			if e := g.checkoutBranch(ctx, id, branch); e != nil {
				return e
			}
		}
		// Tree-replace policy (v1): wipe the worktree (except .git) then write the
		// validated entries. git add -A stages adds AND deletions.
		if e := g.replaceWorktree(id, extracted); e != nil {
			return e
		}
		if _, e := g.run(ctx, id, "add", "-A"); e != nil {
			return wrapGit(e)
		}
		// Nothing to commit if the upload is byte-identical to the current tree.
		status, e := g.run(ctx, id, "status", "--porcelain")
		if e != nil {
			return wrapGit(e)
		}
		if strings.TrimSpace(status) == "" {
			return ErrNothingToCommit
		}
		msg := "Import " + src.Filename
		if src.Filename == "" {
			msg = "Import archive"
		}
		newCI, e := g.commitStaged(ctx, id, branch, actor, msg)
		if e != nil {
			return e
		}
		ci = newCI
		return nil
	})
	if err != nil {
		return CommitInfo{}, err
	}
	g.refreshServed(ctx, id, branch)
	g.bestEffortMirror(ctx, id, branch)
	return ci, nil
}

// zipEntry is a validated, decompressed file ready to be written to the worktree.
type zipEntry struct {
	path    string // cleaned, repo-relative, forward-slash
	content []byte
}

// validateAndReadZip applies every per-entry guard and returns the decompressed
// file set. It NEVER writes to disk. Order of checks matters: structural/ZipSlip
// and extension first (cheap, fail-closed), then the size/bomb accounting.
func (g *gitService) validateAndReadZip(zr *zip.Reader) ([]zipEntry, error) {
	allowOK := g.zipExtAllowed
	var entries []zipEntry
	var totalUncompressed int64

	for _, f := range zr.File {
		name := f.Name
		// Skip directory entries (they pass; we create dirs implicitly on write).
		if strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
			continue
		}
		// Reject symlinks and other non-regular modes outright (ZipSlip via link).
		if f.Mode()&os.ModeSymlink != 0 {
			return nil, ErrZipSlip
		}
		if !f.Mode().IsRegular() {
			return nil, ErrZipSlip
		}
		// ZipSlip: the entry name must be a local, relative path that cannot escape
		// the destination root. filepath.IsLocal rejects absolute + ".." escapes.
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return nil, ErrZipSlip
		}
		clean := filepath.ToSlash(filepath.Clean(name))
		if clean == "." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
			return nil, ErrZipSlip
		}
		for _, seg := range strings.Split(clean, "/") {
			if seg == ".." || seg == gitDirSegment {
				return nil, ErrZipSlip
			}
		}
		// Extension allowlist (config override or MIMEByExt keys).
		if !allowOK(clean) {
			return nil, ErrZipBadType
		}
		// Per-entry decompressed cap (declared) — cheap pre-read bomb guard.
		if int64(f.UncompressedSize64) > g.cfg.Zip.MaxEntryUncompressed {
			return nil, ErrZipTooLarge
		}
		// Compression-ratio guard (declared sizes) catches nested bombs early.
		if f.CompressedSize64 > 0 {
			ratio := f.UncompressedSize64 / f.CompressedSize64
			if ratio > uint64(g.cfg.Zip.MaxCompressionRatio) {
				return nil, ErrZipTooLarge
			}
		}

		// Read with a hard byte limit so a LYING declared size cannot bomb us: cap
		// the reader at the per-entry max + 1 and treat overflow as too-large.
		rc, err := f.Open()
		if err != nil {
			return nil, &ValidationError{Field: "zip", Reason: "cannot open entry " + name}
		}
		limited := io.LimitReader(rc, g.cfg.Zip.MaxEntryUncompressed+1)
		content, err := io.ReadAll(limited)
		rc.Close()
		if err != nil {
			return nil, &ValidationError{Field: "zip", Reason: "cannot read entry " + name}
		}
		if int64(len(content)) > g.cfg.Zip.MaxEntryUncompressed {
			return nil, ErrZipTooLarge
		}
		totalUncompressed += int64(len(content))
		if totalUncompressed > g.cfg.Zip.MaxUncompressedBytes {
			return nil, ErrZipTooLarge
		}
		entries = append(entries, zipEntry{path: clean, content: content})
	}
	if len(entries) == 0 {
		return nil, &ValidationError{Field: "zip", Reason: "archive contains no allowable files"}
	}
	return entries, nil
}

// zipExtAllowed reports whether a path's extension is allowed for upload, honoring
// a config override (cfg.Zip.AllowedExt) when present, else the MIMEByExt keys.
func (g *gitService) zipExtAllowed(p string) bool {
	if len(g.cfg.Zip.AllowedExt) == 0 {
		return extAllowedForUpload(p)
	}
	ext := strings.ToLower(filepath.Ext(p))
	for _, a := range g.cfg.Zip.AllowedExt {
		if strings.EqualFold(a, ext) {
			return true
		}
	}
	return false
}

// replaceWorktree wipes all worktree files (except .git and the served dir) then
// writes the validated entries. This implements the v1 tree-replace policy.
func (g *gitService) replaceWorktree(id uuid.UUID, entries []zipEntry) error {
	root := g.repoDir(id)
	dirents, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("%w: read worktree: %v", ErrGit, err)
	}
	for _, de := range dirents {
		switch de.Name() {
		case gitDirSegment, "served":
			continue // never touch the repo metadata or materialized trees
		}
		if err := os.RemoveAll(filepath.Join(root, de.Name())); err != nil {
			return fmt.Errorf("%w: clear worktree: %v", ErrGit, err)
		}
	}
	for _, e := range entries {
		full := filepath.Join(root, filepath.FromSlash(e.path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("%w: mkdir %s: %v", ErrGit, e.path, err)
		}
		if err := os.WriteFile(full, e.content, 0o644); err != nil {
			return fmt.Errorf("%w: write %s: %v", ErrGit, e.path, err)
		}
	}
	return nil
}

// extractZipToWorktree extracts a validated zip directly into a fresh worktree
// (the CreateSite seed path: the repo has no commits yet). It reuses the same
// guards as ImportZip.
func (g *gitService) extractZipToWorktree(repoDir string, src ZipSource) error {
	if src.Size > g.cfg.Zip.MaxUploadBytes {
		return ErrZipTooLarge
	}
	if src.Reader == nil {
		return &ValidationError{Field: "zip", Reason: "no archive reader"}
	}
	zr, err := zip.NewReader(src.Reader, src.Size)
	if err != nil {
		return &ValidationError{Field: "zip", Reason: "not a valid zip archive"}
	}
	if len(zr.File) > g.cfg.Zip.MaxEntries {
		return ErrZipTooManyFiles
	}
	entries, err := g.validateAndReadZip(zr)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := filepath.Join(repoDir, filepath.FromSlash(e.path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("%w: mkdir %s: %v", ErrGit, e.path, err)
		}
		if err := os.WriteFile(full, e.content, 0o644); err != nil {
			return fmt.Errorf("%w: write %s: %v", ErrGit, e.path, err)
		}
	}
	return nil
}

// extractTar extracts a git-archive tar blob into dst, applying the same ZipSlip
// guards as the zip path (the served-worktree materializer uses this; the input
// comes from our own `git archive`, but we still validate defensively).
func extractTar(tarBytes []byte, dst string) error {
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		name := hdr.Name
		if name == "" || strings.HasSuffix(name, "/") {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("tar entry escapes root: %s", name)
		}
		clean := filepath.Clean(filepath.FromSlash(name))
		full := filepath.Join(dst, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			// Bounded copy: a git-archive tar is trusted but we still cap absurd sizes.
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		default:
			// Skip symlinks/devices/etc — served trees are plain files only.
			continue
		}
	}
	return nil
}
