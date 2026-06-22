package site

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// FakeService is a full in-memory implementation of site.Service honoring the
// SAME contract (optimistic lock, error taxonomy, path/handle validation) as the
// real gitService. Downstream handler/MCP/test code injects it so the control
// plane is unit-testable without git or a database. It is exported so other
// packages (api, mcpserver) can use it in their own tests.
//
// The fake models git just enough to satisfy the contract: each branch is a
// linear commit log of immutable tree snapshots, keyed by a monotonic synthetic
// SHA. The tip SHA is the optimistic-lock token, exactly as in the real impl.
type FakeService struct {
	mu    sync.Mutex
	sites map[uuid.UUID]*fakeSite
	// redirects maps an old handle -> the current site (rename history).
	redirects map[string]uuid.UUID
	// seq feeds the synthetic SHA generator so every commit has a unique, stable id.
	seq   int
	clock func() time.Time

	// FailNext forces an error from the named method ONCE (method name -> error),
	// for deterministically exercising error branches in handler tests.
	FailNext map[string]error
}

type fakeSite struct {
	rec      SiteRecord
	branches map[BranchName]*fakeBranch
	// remote is the configured mirror URL ("" = none); MirrorPush no-ops in the fake.
	remote string
}

type fakeBranch struct {
	commits []fakeCommit // append-only; last element is the tip
}

type fakeCommit struct {
	sha     string
	parents []string
	message string
	author  string
	email   string
	via     string
	when    time.Time
	tree    map[string][]byte // path -> content snapshot for this commit
}

// NewFakeService builds an empty in-memory Service.
func NewFakeService() *FakeService {
	return &FakeService{
		sites:     make(map[uuid.UUID]*fakeSite),
		redirects: make(map[string]uuid.UUID),
		clock:     time.Now,
		FailNext:  make(map[string]error),
	}
}

// NewFakeServiceWithClock is NewFakeService with an injectable clock (tests).
func NewFakeServiceWithClock(clock func() time.Time) *FakeService {
	f := NewFakeService()
	if clock != nil {
		f.clock = clock
	}
	return f
}

// compile-time guarantee FakeService implements the frozen interface.
var _ Service = (*FakeService)(nil)

// failOnce consumes a queued FailNext error for method, if present.
func (f *FakeService) failOnce(method string) error {
	if f.FailNext == nil {
		return nil
	}
	if err, ok := f.FailNext[method]; ok {
		delete(f.FailNext, method)
		return err
	}
	return nil
}

// nextSHA returns the next synthetic 40-hex commit id (monotonic, deterministic).
func (f *FakeService) nextSHA() string {
	f.seq++
	return fmt.Sprintf("%040x", f.seq)
}

// tip returns the current tip commit of a branch (the last in its log), or false.
func (b *fakeBranch) tip() (fakeCommit, bool) {
	if len(b.commits) == 0 {
		return fakeCommit{}, false
	}
	return b.commits[len(b.commits)-1], true
}

// toCommitInfo converts an internal fakeCommit to the public CommitInfo.
func (c fakeCommit) toCommitInfo() CommitInfo {
	short := c.sha
	if len(short) > 7 {
		short = short[:7]
	}
	return CommitInfo{
		SHA:         c.sha,
		ShortSHA:    short,
		Message:     c.message,
		AuthorName:  c.author,
		AuthorEmail: c.email,
		Committed:   c.when,
		Parents:     c.parents,
		Via:         c.via,
	}
}

// cloneTree deep-copies a tree snapshot so commits remain immutable.
func cloneTree(src map[string][]byte) map[string][]byte {
	dst := make(map[string][]byte, len(src))
	for k, v := range src {
		cp := make([]byte, len(v))
		copy(cp, v)
		dst[k] = cp
	}
	return dst
}

// commit appends a new commit on the branch with the given tree + actor + msg,
// returning its CommitInfo. The caller holds f.mu.
func (f *FakeService) commit(b *fakeBranch, tree map[string][]byte, actor Actor, msg string) CommitInfo {
	var parents []string
	if t, ok := b.tip(); ok {
		parents = []string{t.sha}
	}
	via := string(actor.Via)
	if via == "" {
		via = string(SourceSystem)
	}
	email := actor.Email
	if email == "" {
		email = actor.UserID.String() + "@kotoji.local"
	}
	name := actor.Name
	if name == "" {
		name = "kotoji"
	}
	c := fakeCommit{
		sha:     f.nextSHA(),
		parents: parents,
		message: msg,
		author:  name,
		email:   email,
		via:     via,
		when:    f.clock().UTC(),
		tree:    cloneTree(tree),
	}
	b.commits = append(b.commits, c)
	return c.toCommitInfo()
}

// liveSite fetches a non-deleted site, or ErrNotFound. The caller holds f.mu.
func (f *FakeService) liveSite(id uuid.UUID) (*fakeSite, error) {
	s, ok := f.sites[id]
	if !ok || s.rec.DeletedAt != nil {
		return nil, ErrNotFound
	}
	return s, nil
}

// ---- Site lifecycle ----

func (f *FakeService) CreateSite(ctx context.Context, in CreateSiteInput) (Site, error) {
	if err := ValidateHandle(in.Handle); err != nil {
		return Site{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOnce("CreateSite"); err != nil {
		return Site{}, err
	}

	lower := strings.ToLower(string(in.Handle))
	// Collision: existing live site, redirect, or reserved word.
	for _, s := range f.sites {
		if s.rec.DeletedAt == nil && s.rec.Handle == lower {
			return Site{}, ErrHandleTaken
		}
	}
	if _, ok := f.redirects[lower]; ok {
		return Site{}, ErrHandleTaken
	}

	visibility := in.Visibility
	if visibility == "" {
		visibility = "private"
	}
	publishMode := in.PublishMode
	if publishMode == "" {
		publishMode = "direct"
	}
	now := f.clock().UTC()
	id := uuid.New()
	site := &fakeSite{
		rec: SiteRecord{
			ID:            id,
			Handle:        lower,
			OwnerID:       in.OwnerID,
			Visibility:    visibility,
			DefaultBranch: string(BranchDraft),
			PublishMode:   publishMode,
			GitHubRepo:    emptyToNil(in.GitHubRepo),
			Description:   in.Description,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		branches: map[BranchName]*fakeBranch{},
		remote:   in.GitHubRepo,
	}

	// Seed the draft branch with the initial tree.
	tree := map[string][]byte{}
	if in.Zip != nil {
		entries, err := f.readZip(in.Zip)
		if err != nil {
			return Site{}, err
		}
		for _, e := range entries {
			tree[e.path] = e.content
		}
	} else {
		tree["index.html"] = []byte(defaultIndexHTML(string(in.Handle)))
	}
	draft := &fakeBranch{}
	msg := "Initial commit"
	if in.Zip != nil && in.Zip.Filename != "" {
		msg = "Import " + in.Zip.Filename
	}
	f.commit(draft, tree, in.Actor, msg)
	site.branches[BranchDraft] = draft

	f.sites[id] = site
	return site.rec.toSite(), nil
}

// readZip applies the SAME guards as the real impl (delegates to the shared
// validator) so the fake rejects ZipSlip/bomb/bad-type identically.
func (f *FakeService) readZip(src *ZipSource) ([]zipEntry, error) {
	// Reuse the production validator via a throwaway gitService with default cfg.
	g := &gitService{cfg: Config{}.withDefaults()}
	if src.Size > g.cfg.Zip.MaxUploadBytes {
		return nil, ErrZipTooLarge
	}
	if src.Reader == nil {
		return nil, &ValidationError{Field: "zip", Reason: "no archive reader"}
	}
	zr, err := zipNewReader(src.Reader, src.Size)
	if err != nil {
		return nil, &ValidationError{Field: "zip", Reason: "not a valid zip archive"}
	}
	if len(zr.File) > g.cfg.Zip.MaxEntries {
		return nil, ErrZipTooManyFiles
	}
	return g.validateAndReadZip(zr)
}

func (f *FakeService) GetSite(ctx context.Context, id uuid.UUID) (Site, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return Site{}, err
	}
	return s.rec.toSite(), nil
}

func (f *FakeService) GetSiteByHandle(ctx context.Context, h Handle) (Site, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lower := strings.ToLower(string(h))
	for _, s := range f.sites {
		if s.rec.DeletedAt == nil && s.rec.Handle == lower {
			return s.rec.toSite(), nil
		}
	}
	return Site{}, ErrNotFound
}

func (f *FakeService) ListSites(ctx context.Context, ownerID uuid.UUID) ([]Site, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Site
	for _, s := range f.sites {
		if s.rec.DeletedAt != nil {
			continue
		}
		// An empty ownerID lists all (admin path); otherwise filter by owner.
		if ownerID == uuid.Nil || s.rec.OwnerID == ownerID {
			out = append(out, s.rec.toSite())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (f *FakeService) RenameHandle(ctx context.Context, id uuid.UUID, newHandle Handle) (Site, error) {
	if err := ValidateHandle(newHandle); err != nil {
		return Site{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return Site{}, err
	}
	lower := strings.ToLower(string(newHandle))
	if lower == s.rec.Handle {
		return s.rec.toSite(), nil
	}
	// Collision unless it is a rename-back redirect of THIS site.
	for _, other := range f.sites {
		if other.rec.DeletedAt == nil && other.rec.Handle == lower {
			return Site{}, ErrHandleTaken
		}
	}
	if owner, ok := f.redirects[lower]; ok && owner != id {
		return Site{}, ErrHandleTaken
	}
	// Record the old handle as a redirect, drop a rename-back redirect, update live.
	delete(f.redirects, lower) // rename-back: free the redirect we are reclaiming
	f.redirects[s.rec.Handle] = id
	s.rec.Handle = lower
	s.rec.UpdatedAt = f.clock().UTC()
	return s.rec.toSite(), nil
}

func (f *FakeService) DeleteSite(ctx context.Context, id uuid.UUID, actor Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return err
	}
	now := f.clock().UTC()
	s.rec.DeletedAt = &now
	s.rec.UpdatedAt = now
	return nil
}

// ---- Branches ----

func (f *FakeService) ListBranches(ctx context.Context, id uuid.UUID) ([]Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return nil, err
	}
	names := make([]BranchName, 0, len(s.branches))
	for n := range s.branches {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	var out []Branch
	for _, n := range names {
		b := s.branches[n]
		tip, ok := b.tip()
		if !ok {
			continue
		}
		out = append(out, Branch{
			Name:             n,
			HeadSHA:          tip.sha,
			IsPublished:      n == BranchPublished,
			LastCommit:       tip.toCommitInfo(),
			PreviewSubdomain: previewSubdomain(Handle(s.rec.Handle), n),
		})
	}
	return out, nil
}

func (f *FakeService) CreateBranch(ctx context.Context, id uuid.UUID, name BranchName, from string) (Branch, error) {
	if err := validateBranchName(name); err != nil {
		return Branch{}, err
	}
	if name == BranchPublished || name == BranchDraft {
		return Branch{}, &ValidationError{Field: "branch", Reason: "reserved branch name"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return Branch{}, err
	}
	if _, ok := s.branches[name]; ok {
		return Branch{}, ErrBranchExists
	}
	fromBranch := BranchName(from)
	if from == "" {
		fromBranch = BranchDraft
	}
	src, srcCommit, ok := f.resolveRef(s, string(fromBranch))
	_ = src
	if !ok {
		return Branch{}, ErrNotFound
	}
	// Copy the source branch's full log so history is shared up to the branch point.
	nb := &fakeBranch{commits: append([]fakeCommit(nil), commitsUpTo(s, srcCommit.sha)...)}
	s.branches[name] = nb
	return Branch{
		Name:             name,
		HeadSHA:          srcCommit.sha,
		IsPublished:      false,
		LastCommit:       srcCommit.toCommitInfo(),
		PreviewSubdomain: previewSubdomain(Handle(s.rec.Handle), name),
	}, nil
}

func (f *FakeService) DeleteBranch(ctx context.Context, id uuid.UUID, name BranchName) error {
	if name == BranchPublished || name == BranchDraft {
		return &ValidationError{Field: "branch", Reason: "cannot delete the published or draft branch"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return err
	}
	if _, ok := s.branches[name]; !ok {
		return ErrNotFound
	}
	delete(s.branches, name)
	return nil
}

// ---- Files (read) ----

func (f *FakeService) ListFiles(ctx context.Context, in ListFilesInput) ([]FileEntry, ResolvedRef, error) {
	if err := validateDir(in.Dir); err != nil {
		return nil, ResolvedRef{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(in.SiteID)
	if err != nil {
		return nil, ResolvedRef{}, err
	}
	commit, ok := f.commitForRefOrBranch(s, in.Ref, in.Branch)
	if !ok {
		return nil, ResolvedRef{}, ErrNotFound
	}
	entries := listTree(commit.tree, in.Dir, in.Recursive)
	return entries, ResolvedRef{SHA: commit.sha}, nil
}

func (f *FakeService) ReadFile(ctx context.Context, id uuid.UUID, branch BranchName, ref, path string) (FileContent, error) {
	if err := validateReadPath(path); err != nil {
		return FileContent{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return FileContent{}, err
	}
	commit, ok := f.commitForRefOrBranch(s, ref, branch)
	if !ok {
		return FileContent{}, ErrNotFound
	}
	content, ok := commit.tree[path]
	if !ok {
		return FileContent{}, ErrNotFound
	}
	cp := make([]byte, len(content))
	copy(cp, content)
	return FileContent{
		Path:     path,
		Content:  cp,
		SHA:      commit.sha,
		BlobSHA:  fmt.Sprintf("%040x", len(content)), // synthetic but stable-ish blob id
		Size:     int64(len(content)),
		IsBinary: isBinaryContent(content),
	}, nil
}

// ---- Files (write) ----

func (f *FakeService) WriteFile(ctx context.Context, in WriteFileInput) (CommitInfo, error) {
	if err := refuseProtectedBranch(in.Branch); err != nil {
		return CommitInfo{}, err
	}
	var pathErr error
	if in.Actor.Via == SourceMCP {
		pathErr = validateMCPWritePath(in.Path)
	} else {
		pathErr = validatePath(in.Path)
	}
	if pathErr != nil {
		return CommitInfo{}, pathErr
	}
	if in.BaseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required (never treated as force)"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(in.SiteID)
	if err != nil {
		return CommitInfo{}, err
	}
	b, ok := s.branches[in.Branch]
	if !ok {
		return CommitInfo{}, ErrNotFound
	}
	if err := f.checkBaseSHA(b, in.Branch, in.BaseSHA, []string{in.Path}); err != nil {
		return CommitInfo{}, err
	}
	tip, _ := b.tip()
	tree := cloneTree(tip.tree)
	tree[in.Path] = append([]byte(nil), in.Content...)
	msg := in.Message
	if msg == "" {
		msg = "Update " + in.Path
	}
	ci := f.commit(b, tree, in.Actor, msg)
	s.rec.UpdatedAt = f.clock().UTC()
	return ci, nil
}

func (f *FakeService) DeleteFile(ctx context.Context, id uuid.UUID, branch BranchName, path, baseSHA string, actor Actor) (CommitInfo, error) {
	if err := refuseProtectedBranch(branch); err != nil {
		return CommitInfo{}, err
	}
	if err := validateReadPath(path); err != nil {
		return CommitInfo{}, err
	}
	if baseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required (never treated as force)"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return CommitInfo{}, err
	}
	b, ok := s.branches[branch]
	if !ok {
		return CommitInfo{}, ErrNotFound
	}
	if err := f.checkBaseSHA(b, branch, baseSHA, []string{path}); err != nil {
		return CommitInfo{}, err
	}
	tip, _ := b.tip()
	if _, ok := tip.tree[path]; !ok {
		return CommitInfo{}, ErrNotFound
	}
	tree := cloneTree(tip.tree)
	delete(tree, path)
	ci := f.commit(b, tree, actor, "Delete "+path)
	s.rec.UpdatedAt = f.clock().UTC()
	return ci, nil
}

func (f *FakeService) Commit(ctx context.Context, in CommitInput) (CommitInfo, error) {
	if err := refuseProtectedBranch(in.Branch); err != nil {
		return CommitInfo{}, err
	}
	if in.BaseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required (never treated as force)"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(in.SiteID)
	if err != nil {
		return CommitInfo{}, err
	}
	b, ok := s.branches[in.Branch]
	if !ok {
		return CommitInfo{}, ErrNotFound
	}
	if err := f.checkBaseSHA(b, in.Branch, in.BaseSHA, nil); err != nil {
		return CommitInfo{}, err
	}
	// The fake has no separate staging area: WriteFile commits per call, so a bare
	// Commit with no pending changes is "nothing to commit" (contract parity).
	return CommitInfo{}, ErrNothingToCommit
}

func (f *FakeService) ImportZip(ctx context.Context, id uuid.UUID, branch BranchName, src ZipSource, baseSHA string, actor Actor) (CommitInfo, error) {
	if err := refuseProtectedBranch(branch); err != nil {
		return CommitInfo{}, err
	}
	entries, err := f.readZip(&src)
	if err != nil {
		return CommitInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return CommitInfo{}, err
	}
	b, exists := s.branches[branch]
	if exists {
		if baseSHA == "" {
			return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required for an existing branch"}
		}
		if err := f.checkBaseSHA(b, branch, baseSHA, nil); err != nil {
			return CommitInfo{}, err
		}
	} else {
		b = &fakeBranch{}
		s.branches[branch] = b
	}
	tree := map[string][]byte{}
	for _, e := range entries {
		tree[e.path] = e.content
	}
	// Nothing to commit if the upload equals the current tree (parity).
	if tip, ok := b.tip(); ok && treesEqual(tip.tree, tree) {
		return CommitInfo{}, ErrNothingToCommit
	}
	msg := "Import " + src.Filename
	if src.Filename == "" {
		msg = "Import archive"
	}
	ci := f.commit(b, tree, actor, msg)
	s.rec.UpdatedAt = f.clock().UTC()
	return ci, nil
}

// ---- Publish ----

func (f *FakeService) Publish(ctx context.Context, in PublishInput) (CommitInfo, error) {
	from := in.From
	if from == "" {
		from = BranchDraft
	}
	if from == BranchPublished {
		return CommitInfo{}, &ValidationError{Field: "from", Reason: "cannot publish from the published branch"}
	}
	if in.BaseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required (tip of the source branch)"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(in.SiteID)
	if err != nil {
		return CommitInfo{}, err
	}
	srcBranch, ok := s.branches[from]
	if !ok {
		return CommitInfo{}, ErrNotFound
	}
	srcTip, ok := srcBranch.tip()
	if !ok {
		return CommitInfo{}, ErrNotFound
	}
	if srcTip.sha != in.BaseSHA {
		return CommitInfo{}, &ConflictError{Branch: from, Expected: in.BaseSHA, Actual: srcTip.sha}
	}
	pub, pubExists := s.branches[BranchPublished]
	if !pubExists {
		// First publish: published gets the source history.
		s.branches[BranchPublished] = &fakeBranch{commits: append([]fakeCommit(nil), srcBranch.commits...)}
		f.setPublished(s, srcTip)
		return srcTip.toCommitInfo(), nil
	}
	pubTip, _ := pub.tip()
	if pubTip.sha == srcTip.sha {
		// Idempotent.
		return pubTip.toCommitInfo(), nil
	}
	// Fast-forward when published's tip is an ancestor of the source tip.
	if isAncestor(srcBranch, pubTip.sha) {
		pub.commits = append([]fakeCommit(nil), srcBranch.commits...)
		f.setPublished(s, srcTip)
		return srcTip.toCommitInfo(), nil
	}
	// Diverged: detect a true content conflict (same path, different bytes, both
	// changed since the merge base) -> ErrPublishConflict; else a merge commit.
	conflicts := conflictingPaths(pubTip.tree, srcTip.tree, mergeBaseTree(s, srcBranch, pubTip.sha))
	if len(conflicts) > 0 {
		return CommitInfo{}, &PublishConflictError{Paths: conflicts}
	}
	// Clean merge: union the trees, new merge commit with two parents.
	merged := mergeTrees(pubTip.tree, srcTip.tree)
	mc := fakeCommit{
		sha:     f.nextSHA(),
		parents: []string{pubTip.sha, srcTip.sha},
		message: publishMergeMessage(from, srcTip.sha, in.Message),
		author:  actorName(in.Actor),
		email:   actorEmail(in.Actor),
		via:     actorVia(in.Actor),
		when:    f.clock().UTC(),
		tree:    cloneTree(merged),
	}
	pub.commits = append(pub.commits, mc)
	f.setPublished(s, mc)
	return mc.toCommitInfo(), nil
}

// setPublished updates the published pointer metadata (parity with the real impl
// updating sites.published_commit_sha / published_at).
func (f *FakeService) setPublished(s *fakeSite, tip fakeCommit) {
	sha := tip.sha
	s.rec.PublishedSHA = &sha
	t := f.clock().UTC()
	s.rec.PublishedAt = &t
	s.rec.UpdatedAt = t
}

// ---- History / diff / rollback ----

func (f *FakeService) GetDiff(ctx context.Context, in DiffOptions) (DiffResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(in.SiteID)
	if err != nil {
		return DiffResult{}, err
	}
	fromCommit, ok := f.resolveAnyRef(s, in.From)
	if !ok {
		return DiffResult{}, ErrNotFound
	}
	var toTree map[string][]byte
	toSHA := ""
	if in.To == "" {
		// No working tree in the fake; diff against From itself yields no changes.
		toTree = fromCommit.tree
	} else {
		toCommit, ok := f.resolveAnyRef(s, in.To)
		if !ok {
			return DiffResult{}, ErrNotFound
		}
		toTree = toCommit.tree
		toSHA = toCommit.sha
	}
	files := diffTrees(fromCommit.tree, toTree, in.Path)
	return DiffResult{FromSHA: fromCommit.sha, ToSHA: toSHA, Files: files}, nil
}

func (f *FakeService) GetLog(ctx context.Context, in LogOptions) ([]CommitInfo, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > defaultMaxLogLimit {
		limit = defaultMaxLogLimit
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(in.SiteID)
	if err != nil {
		return nil, err
	}
	b, ok := s.branches[in.Branch]
	if !ok {
		return nil, ErrNotFound
	}
	// Newest-first.
	var all []fakeCommit
	for i := len(b.commits) - 1; i >= 0; i-- {
		all = append(all, b.commits[i])
	}
	// Apply the Before cursor (strictly older than Before).
	start := 0
	if in.Before != "" {
		for i, c := range all {
			if c.sha == in.Before {
				start = i + 1
				break
			}
		}
	}
	var out []CommitInfo
	for i := start; i < len(all) && len(out) < limit; i++ {
		c := all[i]
		if in.Path != "" {
			// Restrict to commits that changed the path (added/modified/deleted).
			if !commitTouchedPath(b.commits, c.sha, in.Path) {
				continue
			}
		}
		out = append(out, c.toCommitInfo())
	}
	return out, nil
}

func (f *FakeService) Rollback(ctx context.Context, id uuid.UUID, branch BranchName, toSHA, baseSHA string, actor Actor) (CommitInfo, error) {
	if err := refuseProtectedBranch(branch); err != nil {
		return CommitInfo{}, err
	}
	if baseSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "baseSha", Reason: "required"}
	}
	if toSHA == "" {
		return CommitInfo{}, &ValidationError{Field: "toSha", Reason: "required"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return CommitInfo{}, err
	}
	b, ok := s.branches[branch]
	if !ok {
		return CommitInfo{}, ErrNotFound
	}
	if err := f.checkBaseSHA(b, branch, baseSHA, nil); err != nil {
		return CommitInfo{}, err
	}
	// Ancestor-only: toSHA must exist in this branch's history.
	var target *fakeCommit
	for i := range b.commits {
		if b.commits[i].sha == toSHA {
			c := b.commits[i]
			target = &c
			break
		}
	}
	if target == nil {
		return CommitInfo{}, ErrNotFound
	}
	short := toSHA
	if len(short) > 7 {
		short = short[:7]
	}
	// Forward commit whose tree equals the target's tree (history preserved).
	ci := f.commit(b, cloneTree(target.tree), actor, "Rollback to "+short)
	s.rec.UpdatedAt = f.clock().UTC()
	return ci, nil
}

// ---- Data-plane read side ----

func (f *FakeService) ServedTree(ctx context.Context, id uuid.UUID, branch BranchName) (TreeHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return TreeHandle{}, err
	}
	b, ok := s.branches[branch]
	if !ok {
		return TreeHandle{}, ErrNotFound
	}
	tip, ok := b.tip()
	if !ok {
		return TreeHandle{}, ErrNotFound
	}
	// The fake has no on-disk root; Root is "" (callers that need real files use
	// the real gitService). CommitSHA + time back ETag/Last-Modified.
	return TreeHandle{
		Root:       "",
		CommitSHA:  tip.sha,
		CommitTime: tip.when,
		Exists:     true,
	}, nil
}

// ResolveForServing mirrors gitService.ResolveForServing against the in-memory
// site/redirect maps. Not part of the frozen interface (kept for data-plane test
// parity). Errors: ErrNotFound, ErrValidation.
func (f *FakeService) ResolveForServing(ctx context.Context, hostOrPath string) (ServeTarget, error) {
	handle, branch, err := parseHostOrPath(hostOrPath)
	if err != nil {
		return ServeTarget{}, err
	}
	if branch == BranchPublished && strings.Contains(hostOrPath, doubleHyphen) {
		return ServeTarget{}, &ValidationError{Field: "branch", Reason: "published is not addressable via --"}
	}
	if err := ValidateHandleForResolver(handle); err != nil {
		return ServeTarget{}, err
	}
	if branch != BranchPublished {
		if err := validateBranchName(branch); err != nil {
			return ServeTarget{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	lower := strings.ToLower(string(handle))
	var target *fakeSite
	for _, s := range f.sites {
		if s.rec.DeletedAt == nil && s.rec.Handle == lower {
			target = s
			break
		}
	}
	if target == nil {
		if owner, ok := f.redirects[lower]; ok {
			target = f.sites[owner]
		}
	}
	if target == nil || target.rec.DeletedAt != nil {
		return ServeTarget{}, ErrNotFound
	}
	b, ok := target.branches[branch]
	if !ok {
		return ServeTarget{}, ErrNotFound
	}
	tip, ok := b.tip()
	if !ok {
		return ServeTarget{}, ErrNotFound
	}
	return ServeTarget{
		SiteID:  target.rec.ID,
		Handle:  Handle(target.rec.Handle),
		Branch:  branch,
		Root:    "",
		HeadSHA: tip.sha,
	}, nil
}

// ---- GitHub mirror / remote (no-op / metadata-only in the fake) ----

func (f *FakeService) SetRemote(ctx context.Context, id uuid.UUID, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return err
	}
	s.remote = url
	s.rec.GitHubRepo = emptyToNil(url)
	return nil
}

func (f *FakeService) MirrorPush(ctx context.Context, id uuid.UUID, branches ...BranchName) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.liveSite(id); err != nil {
		return err
	}
	// FailNext lets handler tests exercise the best-effort push-failure branch
	// (origin unreachable / auth rejected) without a real remote.
	if err := f.failOnce("MirrorPush"); err != nil {
		return err
	}
	// The fake has no remote to push to; success is the best-effort contract.
	return nil
}

func (f *FakeService) FetchAndUpdate(ctx context.Context, id uuid.UUID, branch BranchName) (CommitInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := f.liveSite(id)
	if err != nil {
		return CommitInfo{}, err
	}
	b, ok := s.branches[branch]
	if !ok {
		return CommitInfo{}, ErrNotFound
	}
	tip, ok := b.tip()
	if !ok {
		return CommitInfo{}, ErrNotFound
	}
	// No real remote in the fake: a fetch is a no-op returning the current tip.
	return tip.toCommitInfo(), nil
}

// ---- fake-internal helpers ----

// checkBaseSHA compares baseSHA to the branch tip, producing a ConflictError with
// changed paths on mismatch (parity with gitService.checkBaseSHA).
func (f *FakeService) checkBaseSHA(b *fakeBranch, branch BranchName, baseSHA string, hint []string) error {
	tip, ok := b.tip()
	if !ok {
		return ErrNotFound
	}
	if tip.sha == baseSHA {
		return nil
	}
	changed := changedBetween(b.commits, baseSHA, tip.sha)
	if len(changed) == 0 {
		changed = hint
	}
	return &ConflictError{Branch: branch, Expected: baseSHA, Actual: tip.sha, ChangedPaths: changed}
}

// commitForRefOrBranch resolves a Ref (SHA) if set, else the tip of branch.
func (f *FakeService) commitForRefOrBranch(s *fakeSite, ref string, branch BranchName) (fakeCommit, bool) {
	if ref != "" {
		return f.resolveAnyRef(s, ref)
	}
	b, ok := s.branches[branch]
	if !ok {
		return fakeCommit{}, false
	}
	return b.tip()
}

// resolveRef resolves a branch name to its branch + tip commit.
func (f *FakeService) resolveRef(s *fakeSite, name string) (*fakeBranch, fakeCommit, bool) {
	b, ok := s.branches[BranchName(name)]
	if !ok {
		return nil, fakeCommit{}, false
	}
	tip, ok := b.tip()
	return b, tip, ok
}

// resolveAnyRef resolves either a branch name or a commit SHA to a commit.
func (f *FakeService) resolveAnyRef(s *fakeSite, ref string) (fakeCommit, bool) {
	if b, ok := s.branches[BranchName(ref)]; ok {
		return b.tip()
	}
	for _, b := range s.branches {
		for _, c := range b.commits {
			if c.sha == ref {
				return c, true
			}
		}
	}
	return fakeCommit{}, false
}
