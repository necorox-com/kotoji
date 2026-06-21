package site

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// This file implements the GitHub mirror / remote arm of the Service
// (CANONICAL §1). The locked design rule (site-service.md §6/§9): mirror push is
// BEST-EFFORT — a push failure NEVER fails the originating save/publish (the
// internal callers in git_service.go use bestEffortMirror and swallow errors);
// only the EXPLICIT MirrorPush call below surfaces a warning-bearing error.

// SetRemote configures (or clears, when url=="") the "origin" mirror remote and
// records github_repo in metadata. CANONICAL §1.
func (g *gitService) SetRemote(ctx context.Context, id uuid.UUID, url string) error {
	rec, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found || rec.DeletedAt != nil {
		return ErrNotFound
	}
	return g.withWriteLock(id, func() error {
		if url == "" {
			// Clear: remove the remote (ignore "no such remote") + null the column.
			_ = g.runErr(ctx, id, "remote", "remove", "origin")
			return g.store.SetSiteRemote(ctx, id, nil)
		}
		norm := normalizeRemoteURL(url)
		// `remote add` fails if origin already exists; fall back to set-url so the
		// call is idempotent whether or not a remote was previously configured.
		if e := g.runErr(ctx, id, "remote", "add", "origin", norm); e != nil {
			if _, se := g.run(ctx, id, "remote", "set-url", "origin", norm); se != nil {
				return wrapGit(se)
			}
		}
		repo := url
		return g.store.SetSiteRemote(ctx, id, &repo)
	})
}

// MirrorPush best-effort pushes the given branches to origin (defaults to
// published). Invoked internally by WriteFile/Commit/Publish via bestEffortMirror
// (which discards errors); the EXPLICIT call here returns the error so a manual
// "sync now" can show "GitHub sync failed". CANONICAL §1.
func (g *gitService) MirrorPush(ctx context.Context, id uuid.UUID, branches ...BranchName) error {
	_, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found {
		return ErrNotFound
	}
	if len(branches) == 0 {
		branches = []BranchName{BranchPublished}
	}
	return g.withReadLock(id, func() error {
		args := []string{"push", "--force-with-lease", "origin"}
		for _, b := range branches {
			args = append(args, string(b))
		}
		if _, e := g.run(ctx, id, args...); e != nil {
			return wrapGit(e)
		}
		return nil
	})
}

// FetchAndUpdate is the GitHub-webhook entry point: fetch origin, fast-forward
// the local branch + refresh its served worktree. Non-FF is rejected and flagged
// (ErrPublishConflict), never force-applied. CANONICAL §1.
func (g *gitService) FetchAndUpdate(ctx context.Context, id uuid.UUID, branch BranchName) (CommitInfo, error) {
	_, found, err := g.store.GetSiteByID(ctx, id)
	if err != nil {
		return CommitInfo{}, fmt.Errorf("%w: load site: %v", ErrGit, err)
	}
	if !found {
		return CommitInfo{}, ErrNotFound
	}
	var ci CommitInfo
	err = g.withWriteLock(id, func() error {
		if _, e := g.run(ctx, id, "fetch", "origin", string(branch)); e != nil {
			return wrapGit(e)
		}
		localTip, e := g.revParse(ctx, id, string(branch))
		if e != nil {
			// No local branch yet: create it at the fetched tip (first sync).
			remoteTip, re := g.revParse(ctx, id, "origin/"+string(branch))
			if re != nil {
				return ErrNotFound
			}
			if _, ue := g.run(ctx, id, "update-ref", "refs/heads/"+string(branch), remoteTip); ue != nil {
				return wrapGit(ue)
			}
			ci, e = g.commitInfo(ctx, id, remoteTip)
			if e != nil {
				return e
			}
			g.refreshServed(ctx, id, branch)
			return nil
		}
		remoteTip, e := g.revParse(ctx, id, "origin/"+string(branch))
		if e != nil {
			return ErrNotFound
		}
		if localTip == remoteTip {
			ci, e = g.commitInfo(ctx, id, localTip)
			return e
		}
		// Fast-forward only: non-FF is rejected and flagged (never force-applied).
		if e := g.runErr(ctx, id, "merge-base", "--is-ancestor", localTip, remoteTip); e != nil {
			return &PublishConflictError{Paths: nil}
		}
		if _, e := g.run(ctx, id, "update-ref", "refs/heads/"+string(branch), remoteTip); e != nil {
			return wrapGit(e)
		}
		ci, e = g.commitInfo(ctx, id, remoteTip)
		if e != nil {
			return e
		}
		g.refreshServed(ctx, id, branch)
		if branch == BranchPublished {
			s := remoteTip
			_ = g.store.SetPublished(ctx, id, &s)
		}
		return nil
	})
	if err != nil {
		return CommitInfo{}, err
	}
	return ci, nil
}
