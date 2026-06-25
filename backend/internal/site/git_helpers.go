package site

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// logFormat is the stable git-log pretty format we parse in parseLog. Fields are
// separated by the unit separator (0x1f) and records by a sentinel line so commit
// messages (which may contain newlines) are unambiguous.
const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
	// %H sha, %h short, %an author name, %ae author email, %cI committer ISO date,
	// %P parents (space-separated), %B raw body (message), %(trailers:key=Kotoji-Via,valueonly).
	logFormat = "--pretty=format:%H" + fieldSep + "%h" + fieldSep + "%an" + fieldSep +
		"%ae" + fieldSep + "%cI" + fieldSep + "%P" + fieldSep + "%B" + recordSep
)

// viaKey is the git commit trailer carrying provenance (Actor.Via). It maps 1:1
// to CommitInfo.Via and the audit_log.source enum.
const viaKey = "Kotoji-Via"

// viaTrailer formats the trailer flag for a commit invocation.
func viaTrailer(a Actor) string {
	via := a.Via
	if via == "" {
		via = SourceSystem
	}
	return "--trailer=" + viaKey + ": " + string(via)
}

// requireSite loads + liveness-checks a site, returning ErrNotFound for missing
// or soft-deleted rows. Used at the top of every git method so a stale UUID never
// reaches the git layer.
func (g *gitService) requireSite(ctx context.Context, id uuid.UUID) (SiteRecord, error) {
	rec, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return SiteRecord{}, fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found || rec.DeletedAt != nil {
		return SiteRecord{}, ErrNotFound
	}
	return rec, nil
}

// runErr runs a git command and returns only its error (used for predicate
// commands like merge-base --is-ancestor where stdout is irrelevant).
func (g *gitService) runErr(ctx context.Context, id uuid.UUID, args ...string) error {
	_, err := g.git.Run(ctx, g.repoDir(id), nil, args...)
	return err
}

// checkBaseSHA compares the supplied baseSHA to the CURRENT tip of branch under
// the (already-held) write lock. Equal => proceed. Not equal => ConflictError
// carrying Expected/Actual + the changed paths between them (CANONICAL §3/§5).
func (g *gitService) checkBaseSHA(ctx context.Context, id uuid.UUID, branch BranchName, baseSHA string, hint []string) error {
	tip, err := g.revParse(ctx, id, string(branch))
	if err != nil {
		return ErrNotFound
	}
	if tip == baseSHA {
		return nil
	}
	changed := g.changedPaths(ctx, id, baseSHA, tip)
	if len(changed) == 0 {
		changed = hint
	}
	return &ConflictError{Branch: branch, Expected: baseSHA, Actual: tip, ChangedPaths: changed}
}

// changedPaths lists the files that differ between two commits (for the conflict
// payload). Best-effort: on any git error it returns nil so a conflict is still
// reported with empty ChangedPaths.
func (g *gitService) changedPaths(ctx context.Context, id uuid.UUID, a, b string) []string {
	out, err := g.run(ctx, id, "diff", "--name-only", a, b)
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// checkoutBranch makes branch the current HEAD so worktree edits land on it. It
// is a no-op when already on branch. The branch must already exist.
func (g *gitService) checkoutBranch(ctx context.Context, id uuid.UUID, branch BranchName) error {
	cur, err := g.run(ctx, id, "symbolic-ref", "--short", "HEAD")
	if err == nil && strings.TrimSpace(cur) == string(branch) {
		return nil
	}
	if _, err := g.run(ctx, id, "checkout", string(branch)); err != nil {
		return wrapGit(err)
	}
	return nil
}

// commitStaged commits the currently-staged index on branch with the Actor's
// identity + provenance trailer, returning the new commit's CommitInfo. It
// assumes branch is checked out and the write lock is held.
func (g *gitService) commitStaged(ctx context.Context, id uuid.UUID, branch BranchName, actor Actor, msg string) (CommitInfo, error) {
	if _, err := g.runEnv(ctx, id, actor, nil, "commit", "-m", msg, viaTrailer(actor)); err != nil {
		return CommitInfo{}, wrapGit(err)
	}
	tip, err := g.revParse(ctx, id, string(branch))
	if err != nil {
		return CommitInfo{}, wrapGit(err)
	}
	return g.commitInfo(ctx, id, tip)
}

// commitInfo loads a single commit's metadata.
func (g *gitService) commitInfo(ctx context.Context, id uuid.UUID, sha string) (CommitInfo, error) {
	out, err := g.run(ctx, id, "show", "-s", logFormat, sha)
	if err != nil {
		return CommitInfo{}, wrapGit(err)
	}
	commits := parseLog(out)
	if len(commits) == 0 {
		return CommitInfo{}, fmt.Errorf("%w: empty commit info for %s", ErrGit, sha)
	}
	return commits[0], nil
}

// mergeIntoPublished creates a merge commit on the published branch bringing in
// the source tip. On a content conflict it returns the conflicting paths so the
// caller can raise ErrPublishConflict; on success it returns the merge SHA. The
// merge runs in a dedicated temporary worktree so it never disturbs the user's
// checked-out branch or the served trees.
func (g *gitService) mergeIntoPublished(ctx context.Context, id uuid.UUID, from BranchName, srcTip string, actor Actor, message string) (string, []string, error) {
	wt, err := os.MkdirTemp("", "kotoji-merge-*")
	if err != nil {
		return "", nil, fmt.Errorf("%w: merge tmp: %v", ErrGit, err)
	}
	defer os.RemoveAll(wt)
	defer func() { _, _ = g.run(ctx, id, "worktree", "prune") }()

	// Attach a worktree checked out on published, then merge the source tip in.
	if _, e := g.run(ctx, id, "worktree", "add", "-f", wt, string(BranchPublished)); e != nil {
		return "", nil, wrapGit(e)
	}
	defer func() { _, _ = g.run(ctx, id, "worktree", "remove", "--force", wt) }()

	msg := message
	if msg == "" {
		short := srcTip
		if len(short) > 7 {
			short = short[:7]
		}
		msg = "publish: " + string(from) + "@" + short
	}
	when := g.clock().UTC().Format(time.RFC3339)
	env := authorEnv(actor, when)
	var mErr error
	if er, ok := g.git.(envRunner); ok {
		_, mErr = er.RunEnv(ctx, wt, env, nil, "merge", "--no-ff", "-m", msg, srcTip)
	} else {
		_, mErr = g.git.Run(ctx, wt, nil, "merge", "--no-ff", "-m", msg, srcTip)
	}
	if mErr != nil {
		// Conflicted paths come from ls-files -u (unmerged) in the worktree.
		conflicts := g.conflictPaths(ctx, wt)
		// Abort to leave published clean.
		_, _ = g.git.Run(ctx, wt, nil, "merge", "--abort")
		return "", conflicts, wrapGit(mErr)
	}
	// Resolve the new published tip from the main repo (the ref moved).
	mergeSHA, e := g.revParse(ctx, id, string(BranchPublished))
	if e != nil {
		return "", nil, e
	}
	return mergeSHA, nil, nil
}

// conflictPaths lists unmerged paths in a worktree after a failed merge. It
// parses the CLASSIC `git ls-files -u` format ("<mode> <sha> <stage>\t<path>")
// rather than the 2.38+ "--format" flag, for portability with older git.
func (g *gitService) conflictPaths(ctx context.Context, wt string) []string {
	out, err := g.git.Run(ctx, wt, nil, "ls-files", "-u")
	if err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// The path follows the TAB; the metadata (mode sha stage) precedes it.
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		path := line[tab+1:]
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

// ---- served-worktree materialization ----

// ensureServed materializes (or refreshes) the read-only served worktree for a
// branch at sha, using an atomic build-temp + rename swap so readers never see a
// half-written tree (routing-and-serving.md §7.2). The .git is excluded by
// construction (we export the tree via archive | extract, not a checkout).
func (g *gitService) ensureServed(ctx context.Context, id uuid.UUID, branch BranchName, sha string) error {
	dst := g.servedDir(id, branch)
	// If the sidecar records the current sha, the tree is already fresh.
	if meta, err := os.ReadFile(filepath.Join(dst, kotojiMetaFile)); err == nil {
		if strings.TrimSpace(string(meta)) == sha {
			return nil
		}
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("%w: served mkdir: %v", ErrGit, err)
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-served-*")
	if err != nil {
		return fmt.Errorf("%w: served tmp: %v", ErrGit, err)
	}
	// On any failure below, clean the temp dir.
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()

	// `git archive` streams the tree as a tar (NO .git), which we extract into the
	// temp dir. This guarantees the served root excludes repo metadata by
	// construction (routing-and-serving.md §7.2) without a checkout.
	tarBytes, err := g.git.Run(ctx, g.repoDir(id), nil, "archive", "--format=tar", sha)
	if err != nil {
		return wrapGit(err)
	}
	if err := extractTar(tarBytes, tmp); err != nil {
		return fmt.Errorf("%w: served extract: %v", ErrGit, err)
	}

	// Write the sidecar with the commit sha so the next call can skip a rebuild.
	if err := os.WriteFile(filepath.Join(tmp, kotojiMetaFile), []byte(sha), 0o644); err != nil {
		return fmt.Errorf("%w: served meta: %v", ErrGit, err)
	}

	// Atomic swap: remove the old tree, rename the temp into place. (Remove-then-
	// rename is the simplest correct sequence on a single volume; a brief absence
	// window is acceptable for v1 single-replica.)
	_ = os.RemoveAll(dst)
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("%w: served swap: %v", ErrGit, err)
	}
	ok = true
	return nil
}

// kotojiMetaFile is the sidecar holding the served commit SHA (CANONICAL §7.2
// uses a richer JSON; the bare SHA suffices for v1 freshness + ETag keying).
const kotojiMetaFile = ".kotoji-served-sha"

// refreshServed best-effort refreshes the served worktree after a successful
// commit on branch. Failure is logged-by-omission (data plane keeps the old tree)
// — it must never fail the originating write.
func (g *gitService) refreshServed(ctx context.Context, id uuid.UUID, branch BranchName) {
	sha, err := g.revParse(ctx, id, string(branch))
	if err != nil {
		return
	}
	_ = g.ensureServed(ctx, id, branch, sha)
}

// bestEffortMirror pushes a branch to origin when a remote is configured and
// mirroring is enabled. Push failure NEVER fails the originating write
// (CANONICAL §1 MirrorPush contract).
func (g *gitService) bestEffortMirror(ctx context.Context, id uuid.UUID, branch BranchName) {
	if !g.mirrorEnabled(ctx) {
		return
	}
	// Only push when an origin remote exists.
	if err := g.runErr(ctx, id, "remote", "get-url", "origin"); err != nil {
		return
	}
	// runMirror injects the GitHub auth header (env-only, never argv/logged). With
	// no token configured the push is unauthenticated and will simply fail against
	// a private remote; that failure is swallowed here (best-effort), so the
	// originating save/publish still succeeds.
	_, _ = g.runMirror(ctx, id, "push", "--force-with-lease", "origin", string(branch))
}

// ---- parsing helpers ----

// parseLog parses git-log output formatted with logFormat into CommitInfo slices.
func parseLog(out string) []CommitInfo {
	var commits []CommitInfo
	for _, rec := range strings.Split(out, recordSep) {
		rec = strings.TrimLeft(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		fields := strings.SplitN(rec, fieldSep, 7)
		if len(fields) < 7 {
			continue
		}
		ci := CommitInfo{
			SHA:         strings.TrimSpace(fields[0]),
			ShortSHA:    strings.TrimSpace(fields[1]),
			AuthorName:  fields[2],
			AuthorEmail: fields[3],
			Message:     strings.TrimRight(fields[6], "\n"),
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[4])); err == nil {
			ci.Committed = t.UTC()
		}
		if ps := strings.Fields(fields[5]); len(ps) > 0 {
			ci.Parents = ps
		}
		ci.Via = extractVia(ci.Message)
		commits = append(commits, ci)
	}
	return commits
}

// extractVia pulls the Kotoji-Via trailer value out of a commit message body.
func extractVia(message string) string {
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, viaKey+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, viaKey+":"))
		}
	}
	return ""
}

// parseLsTree parses `git ls-tree --long [-r]` output into FileEntry slices.
// Each line: "<mode> <type> <object>\t<size>\t<path>" — but git uses spaces for
// mode/type/object and a TAB before the path; --long inserts the size before the
// tab. Format is: "<mode> <type> <object> <size>\t<path>".
func parseLsTree(out string) []FileEntry {
	var entries []FileEntry
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		// Split on the TAB separating metadata from the path.
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		meta := line[:tab]
		path := line[tab+1:]
		parts := strings.Fields(meta)
		if len(parts) < 3 {
			continue
		}
		mode := parts[0]
		typ := parts[1]
		var size int64
		if len(parts) >= 4 && parts[3] != "-" {
			size, _ = strconv.ParseInt(parts[3], 10, 64)
		}
		entries = append(entries, FileEntry{
			Path:  path,
			Name:  lastSegment(path),
			IsDir: typ == "tree",
			Size:  size,
			Mode:  mode,
		})
	}
	return entries
}

// lastSegment returns the base name of a forward-slash path.
func lastSegment(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// mergeDiff combines --numstat (additions/deletions/binary) with --name-status
// (status classification) into FileDiff slices keyed by path.
func mergeDiff(numstat, nameStatus string) []FileDiff {
	byPath := map[string]*FileDiff{}
	var order []string

	for _, line := range strings.Split(strings.TrimRight(numstat, "\n"), "\n") {
		if line == "" {
			continue
		}
		// "<adds>\t<dels>\t<path>"; binary files show "-\t-\t<path>".
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		path := fields[2]
		fd := &FileDiff{Path: path}
		if fields[0] == "-" && fields[1] == "-" {
			fd.IsBinary = true
		} else {
			fd.Additions, _ = strconv.Atoi(fields[0])
			fd.Deletions, _ = strconv.Atoi(fields[1])
		}
		byPath[path] = fd
		order = append(order, path)
	}

	for _, line := range strings.Split(strings.TrimRight(nameStatus, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		code := fields[0]
		var path, oldPath, status string
		switch {
		case strings.HasPrefix(code, "R"): // rename: R<score>\t<old>\t<new>
			if len(fields) < 3 {
				continue
			}
			oldPath, path, status = fields[1], fields[2], "renamed"
		case strings.HasPrefix(code, "A"):
			path, status = fields[1], "added"
		case strings.HasPrefix(code, "D"):
			path, status = fields[1], "deleted"
		case strings.HasPrefix(code, "M"):
			path, status = fields[1], "modified"
		default:
			path, status = fields[1], "modified"
		}
		fd, ok := byPath[path]
		if !ok {
			fd = &FileDiff{Path: path}
			byPath[path] = fd
			order = append(order, path)
		}
		fd.Status = status
		fd.OldPath = oldPath
	}

	out := make([]FileDiff, 0, len(order))
	seen := map[string]struct{}{}
	for _, p := range order {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, *byPath[p])
	}
	return out
}

// attachPatches splits a unified-diff blob by "diff --git" headers and attaches
// each file's patch text to the matching FileDiff.
func attachPatches(files []FileDiff, unified string) {
	chunks := splitUnified(unified)
	for i := range files {
		if patch, ok := chunks[files[i].Path]; ok {
			files[i].UnifiedPatch = patch
		}
	}
}

// splitUnified maps each file path to its "diff --git ..." hunk text.
func splitUnified(unified string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(unified, "\n")
	var cur strings.Builder
	var curPath string
	flush := func() {
		if curPath != "" {
			out[curPath] = strings.TrimRight(cur.String(), "\n")
		}
		cur.Reset()
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			curPath = parseDiffGitPath(line)
		}
		if curPath != "" {
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	flush()
	return out
}

// parseDiffGitPath extracts the b/ path from a "diff --git a/x b/y" header.
func parseDiffGitPath(line string) string {
	const marker = " b/"
	if i := strings.LastIndex(line, marker); i >= 0 {
		return line[i+len(marker):]
	}
	return ""
}

// ---- misc utilities ----

// refuseProtectedBranch returns ErrValidation when a write targets the published
// branch (writes/deletes/commits/rollbacks go to working branches only; only
// Publish moves the published ref). CANONICAL §1.
func refuseProtectedBranch(branch BranchName) error {
	if branch == BranchPublished {
		return &ValidationError{Field: "branch", Reason: "cannot write directly to the published branch"}
	}
	if branch == "" {
		return &ValidationError{Field: "branch", Reason: "required"}
	}
	return nil
}

// wrapGit ensures any non-context error carries the ErrGit sentinel. Context
// cancellation/deadline is passed through unchanged so callers can branch on it.
func wrapGit(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, ErrGit) {
		return err
	}
	// A *GitError already Is(ErrGit); anything else gets wrapped.
	var ge *GitError
	if errors.As(err, &ge) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrGit, err)
}

// emptyToNil maps "" to a nil *string (for nullable columns).
func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// normalizeRemoteURL turns an "owner/name" shorthand into a GitHub HTTPS URL;
// a value that already looks like a URL is passed through unchanged.
func normalizeRemoteURL(repo string) string {
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	return "https://github.com/" + strings.TrimSuffix(repo, ".git") + ".git"
}

// isUniqueViolation reports whether err looks like a Postgres unique-constraint
// violation (SQLSTATE 23505), used to map a lost handle race to ErrHandleTaken
// without importing pgconn here (string match on the canonical code).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "23505") ||
		strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// defaultIndexHTML is the placeholder served by a freshly-created empty site.
func defaultIndexHTML(handle string) string {
	return "<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n" +
		"<title>" + htmlEscape(handle) + "</title>\n</head>\n<body>\n" +
		"<main><h1>" + htmlEscape(handle) + "</h1>\n" +
		"<p>This kotoji site is ready. Edit <code>index.html</code> to begin.</p></main>\n" +
		"</body>\n</html>\n"
}

// htmlEscape escapes the minimal set needed for the placeholder title/heading.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// parseHostOrPath splits a Host header value OR a "/host/{label}/..." path into
// (handle, branch) using the CANONICAL §5.3 "--" rule. The base-domain suffix is
// not stripped here (the resolver layer owns config); this helper accepts either
// a bare label, a "{label}.{anything}" host (first label is the project label),
// or a "/host/{label}/..." path.
func parseHostOrPath(hostOrPath string) (Handle, BranchName, error) {
	s := strings.ToLower(strings.TrimSpace(hostOrPath))
	if s == "" {
		return "", "", &ValidationError{Field: "host", Reason: "empty host/path"}
	}
	var label string
	switch {
	case strings.HasPrefix(s, "/host/"):
		rest := strings.TrimPrefix(s, "/host/")
		rest = strings.TrimPrefix(rest, "/")
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			label = rest[:i]
		} else {
			label = rest
		}
	default:
		// Strip a :port then take the first DNS label as the project label.
		host := s
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		if i := strings.IndexByte(host, '.'); i >= 0 {
			label = host[:i]
		} else {
			label = host
		}
	}
	if label == "" {
		return "", "", &ValidationError{Field: "host", Reason: "no project label"}
	}
	handle, branch := splitLabel(label)
	return handle, branch, nil
}

// ---- per-site disk quota (M5) ----

// repoDiskSize measures the CURRENT on-disk byte footprint of a site repo
// (CANONICAL §8.4 / KOTOJI_SITE_QUOTA_BYTES). It walks /data/sites/{uuid} summing
// regular-file sizes but EXCLUDES the materialized `served` trees: those are a
// derived read cache rebuilt from the committed history, not tenant-controlled
// data, so counting them would double-charge a tenant for content they cannot
// directly shrink. The .git object store IS counted (that is the unbounded-growth
// surface the quota guards). A walk error on an individual entry is skipped
// (best-effort, fail-open per-file) but a root-stat failure is returned so the
// caller can decide whether to proceed.
func (g *gitService) repoDiskSize(id uuid.UUID) (int64, error) {
	root := g.repoDir(id)
	servedRoot := filepath.Join(root, "served")
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Skip an entry we cannot stat (e.g. a transient temp dir mid-rename)
			// rather than aborting the whole measurement.
			if path == root {
				return err
			}
			return nil //nolint:nilerr // intentional fail-open on a single entry
		}
		// Exclude the derived served-tree cache from the tenant's charged footprint.
		if d.IsDir() && path == servedRoot {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil // best-effort: a vanished file contributes 0
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("%w: measure repo size: %v", ErrGit, err)
	}
	return total, nil
}

// enforceQuota rejects a write when the site's CURRENT on-disk size plus the
// incoming delta would exceed SiteQuotaBytes. A quota <= 0 disables the check. It
// is called UNDER the write lock, BEFORE the worktree is replaced/committed, so a
// would-be over-quota import never lands on disk. Returns ErrQuotaExceeded on a
// breach (mapped to 413/quota_exceeded by the wire layer).
func (g *gitService) enforceQuota(id uuid.UUID, deltaBytes int64) error {
	quota := g.cfg.SiteQuotaBytes
	if quota <= 0 {
		return nil // quota disabled
	}
	current, err := g.repoDiskSize(id)
	if err != nil {
		return err
	}
	if current+deltaBytes > quota {
		return ErrQuotaExceeded
	}
	return nil
}

// gcRepo reclaims loose/orphaned objects (e.g. blobs left by a tree-replace whose
// old content is no longer referenced) so the disk footprint that the quota gates
// reflects live history rather than churn. It is best-effort: a gc failure must
// never fail the originating write (the objects are simply reclaimed on the next
// pass). `--prune=now` drops unreachable objects immediately; `--quiet` keeps the
// command output clean. Unlike the worktree `prune` at git_helpers.go (which only
// drops stale worktree admin entries), this reclaims the OBJECT store.
func (g *gitService) gcRepo(ctx context.Context, id uuid.UUID) {
	_, _ = g.run(ctx, id, "gc", "--auto", "--quiet", "--prune=now")
}
