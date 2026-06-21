package site

import (
	"archive/zip"
	"bytes"
	"io"
	"sort"
	"strings"
)

// zipNewReader is a tiny indirection so fake.go can open a zip without importing
// archive/zip directly (keeping the fake's imports minimal and the open logic in
// one place shared with the production validator).
func zipNewReader(r io.ReaderAt, size int64) (*zip.Reader, error) {
	return zip.NewReader(r, size)
}

// listTree turns a flat path->content tree map into FileEntry slices for a dir,
// honoring the non-recursive (one-level) vs recursive modes. Directories are
// synthesized from path prefixes.
func listTree(tree map[string][]byte, dir string, recursive bool) []FileEntry {
	prefix := ""
	if dir != "" {
		prefix = strings.TrimSuffix(dir, "/") + "/"
	}
	files := map[string]int64{}    // path -> size for regular files in scope
	dirs := map[string]struct{}{}  // immediate subdirectory names in scope

	for path, content := range tree {
		if prefix != "" && !strings.HasPrefix(path, prefix) {
			continue
		}
		rel := strings.TrimPrefix(path, prefix)
		if rel == "" {
			continue
		}
		if recursive {
			files[path] = int64(len(content))
			// Synthesize intermediate dirs for recursive listings.
			segs := strings.Split(rel, "/")
			for i := 0; i < len(segs)-1; i++ {
				dirs[prefix+strings.Join(segs[:i+1], "/")] = struct{}{}
			}
			continue
		}
		// One-level: either a file directly in dir, or a subdir.
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			dirs[prefix+rel[:i]] = struct{}{}
		} else {
			files[path] = int64(len(content))
		}
	}

	var entries []FileEntry
	for path, size := range files {
		entries = append(entries, FileEntry{
			Path:  path,
			Name:  lastSegment(path),
			IsDir: false,
			Size:  size,
			Mode:  "100644",
		})
	}
	for path := range dirs {
		entries = append(entries, FileEntry{
			Path:  path,
			Name:  lastSegment(path),
			IsDir: true,
			Size:  0,
			Mode:  "040000",
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

// commitsUpTo returns the commit slice of whichever branch contains sha, up to
// and including that commit (for branching off a point). Falls back to whichever
// branch holds the SHA as its head.
func commitsUpTo(s *fakeSite, sha string) []fakeCommit {
	for _, b := range s.branches {
		for i, c := range b.commits {
			if c.sha == sha {
				return append([]fakeCommit(nil), b.commits[:i+1]...)
			}
		}
	}
	return nil
}

// isAncestor reports whether sha appears anywhere in the branch's history (i.e.
// it is reachable from the branch tip), used for the fast-forward check.
func isAncestor(branch *fakeBranch, sha string) bool {
	for _, c := range branch.commits {
		if c.sha == sha {
			return true
		}
	}
	return false
}

// mergeBaseTree finds the tree of the most-recent commit common to the source
// branch and the published tip's history (the merge base), for 3-way conflict
// detection. Falls back to an empty tree when no common ancestor is found.
func mergeBaseTree(s *fakeSite, src *fakeBranch, pubTipSHA string) map[string][]byte {
	// Build the set of SHAs reachable from published's tip.
	pub := s.branches[BranchPublished]
	pubSet := map[string]map[string][]byte{}
	if pub != nil {
		for _, c := range pub.commits {
			pubSet[c.sha] = c.tree
		}
	}
	// Walk source newest-first; the first SHA also present in published's history
	// is the merge base.
	for i := len(src.commits) - 1; i >= 0; i-- {
		if tree, ok := pubSet[src.commits[i].sha]; ok {
			return tree
		}
	}
	return map[string][]byte{}
}

// conflictingPaths returns paths that BOTH sides changed (relative to base) to
// different content — a true 3-way conflict. Paths only one side changed merge
// cleanly and are excluded.
func conflictingPaths(pub, src, base map[string][]byte) []string {
	var conflicts []string
	seen := map[string]struct{}{}
	consider := func(path string) {
		if _, dup := seen[path]; dup {
			return
		}
		seen[path] = struct{}{}
		baseV, baseHad := base[path]
		pubV, pubHad := pub[path]
		srcV, srcHad := src[path]
		pubChanged := pubHad != baseHad || (pubHad && baseHad && !bytes.Equal(pubV, baseV))
		srcChanged := srcHad != baseHad || (srcHad && baseHad && !bytes.Equal(srcV, baseV))
		if pubChanged && srcChanged {
			// Both changed: conflict unless they changed to the SAME content.
			if pubHad && srcHad && bytes.Equal(pubV, srcV) {
				return
			}
			conflicts = append(conflicts, path)
		}
	}
	for p := range pub {
		consider(p)
	}
	for p := range src {
		consider(p)
	}
	sort.Strings(conflicts)
	return conflicts
}

// mergeTrees unions two trees (src wins on overlap where there is no conflict).
// Used only on the clean-merge path after conflictingPaths returned empty.
func mergeTrees(pub, src map[string][]byte) map[string][]byte {
	out := cloneTree(pub)
	for k, v := range src {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// treesEqual reports byte-for-byte tree equality (used for nothing-to-commit).
func treesEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !bytes.Equal(va, vb) {
			return false
		}
	}
	return true
}

// diffTrees computes FileDiff entries between two trees with add/modify/delete
// classification (the fake has no rename detection; renames show as add+delete).
func diffTrees(from, to map[string][]byte, pathFilter string) []FileDiff {
	var files []FileDiff
	inScope := func(p string) bool { return pathFilter == "" || p == pathFilter }

	// Added / modified.
	for p, toV := range to {
		if !inScope(p) {
			continue
		}
		fromV, had := from[p]
		if !had {
			files = append(files, FileDiff{Path: p, Status: "added",
				Additions: lineCount(toV), IsBinary: isBinaryContent(toV),
				UnifiedPatch: synthPatch(p, nil, toV)})
		} else if !bytes.Equal(fromV, toV) {
			files = append(files, FileDiff{Path: p, Status: "modified",
				Additions: lineCount(toV), Deletions: lineCount(fromV),
				IsBinary:     isBinaryContent(fromV) || isBinaryContent(toV),
				UnifiedPatch: synthPatch(p, fromV, toV)})
		}
	}
	// Deleted.
	for p, fromV := range from {
		if !inScope(p) {
			continue
		}
		if _, had := to[p]; !had {
			files = append(files, FileDiff{Path: p, Status: "deleted",
				Deletions: lineCount(fromV), IsBinary: isBinaryContent(fromV),
				UnifiedPatch: synthPatch(p, fromV, nil)})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// lineCount counts newline-delimited lines (best-effort additions/deletions).
func lineCount(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// synthPatch builds a minimal unified-diff-ish patch for the fake so callers that
// display patches have non-empty text (binary content yields "").
func synthPatch(path string, from, to []byte) string {
	if isBinaryContent(from) || isBinaryContent(to) {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("--- a/" + path + "\n+++ b/" + path + "\n")
	for _, line := range splitLines(from) {
		sb.WriteString("-" + line + "\n")
	}
	for _, line := range splitLines(to) {
		sb.WriteString("+" + line + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// changedBetween lists paths that differ between the commit identified by baseSHA
// and the one identified by tipSHA within a single branch's log (for conflict
// payloads). Unknown baseSHA yields nil.
func changedBetween(commits []fakeCommit, baseSHA, tipSHA string) []string {
	var baseTree, tipTree map[string][]byte
	for _, c := range commits {
		if c.sha == baseSHA {
			baseTree = c.tree
		}
		if c.sha == tipSHA {
			tipTree = c.tree
		}
	}
	if baseTree == nil || tipTree == nil {
		return nil
	}
	diffs := diffTrees(baseTree, tipTree, "")
	paths := make([]string, 0, len(diffs))
	for _, d := range diffs {
		paths = append(paths, d.Path)
	}
	return paths
}

// commitTouchedPath reports whether the commit at sha changed path relative to
// its parent (for path-filtered logs).
func commitTouchedPath(commits []fakeCommit, sha, path string) bool {
	var idx = -1
	for i, c := range commits {
		if c.sha == sha {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	cur := commits[idx].tree
	var parent map[string][]byte
	if idx > 0 {
		parent = commits[idx-1].tree
	}
	curV, curHad := cur[path]
	parV, parHad := parent[path]
	if curHad != parHad {
		return true
	}
	if curHad && parHad && !bytes.Equal(curV, parV) {
		return true
	}
	return false
}

// publishMergeMessage builds the merge-commit message (parity with the real impl).
func publishMergeMessage(from BranchName, srcSHA, override string) string {
	if override != "" {
		return override
	}
	short := srcSHA
	if len(short) > 7 {
		short = short[:7]
	}
	return "publish: " + string(from) + "@" + short
}

// actor field helpers with the synthesized-default fallbacks (parity with commit).
func actorName(a Actor) string {
	if a.Name == "" {
		return "kotoji"
	}
	return a.Name
}
func actorEmail(a Actor) string {
	if a.Email == "" {
		return a.UserID.String() + "@kotoji.local"
	}
	return a.Email
}
func actorVia(a Actor) string {
	if a.Via == "" {
		return string(SourceSystem)
	}
	return string(a.Via)
}
