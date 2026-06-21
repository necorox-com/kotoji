// Package webhook implements the GitHub webhook receiver (architecture.md §3f /
// §8.1.12). It verifies the X-Hub-Signature-256 HMAC in constant time, parses
// push events, maps the pushed repo to a kotoji site, and — when the push targets
// the site's published-tracking branch — fast-forwards the local mirror via
// site.Service.FetchAndUpdate (which also refreshes the served worktree and the
// published pointer). Everything else is ignored with a 200 so GitHub does not
// retry. The body is treated as untrusted until the signature verifies.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

// signatureHeader is GitHub's HMAC-SHA256 signature header (the only one we
// accept; the legacy SHA-1 X-Hub-Signature is intentionally NOT honored).
const signatureHeader = "X-Hub-Signature-256"

// eventHeader names the delivered event type. We act only on "push".
const eventHeader = "X-GitHub-Event"

// sigPrefix is the algorithm prefix GitHub prepends to the hex digest.
const sigPrefix = "sha256="

// maxBodyBytes bounds the webhook body to defend against memory exhaustion from a
// hostile (or misconfigured) sender. GitHub push payloads are well under this.
const maxBodyBytes = 5 << 20 // 5 MiB

// refPrefix is the git ref namespace a push delivers ("refs/heads/<branch>").
const refPrefix = "refs/heads/"

// Service is the slice of site.Service the webhook needs: the fast-forward entry
// point. Narrowed to one method so the handler is trivially mockable (FakeService
// satisfies it; tests inject a fake).
type Service interface {
	// FetchAndUpdate fetches origin and fast-forwards the local branch + served
	// worktree (CANONICAL §1). Non-FF is rejected (ErrPublishConflict).
	FetchAndUpdate(ctx context.Context, id uuid.UUID, branch site.BranchName) (site.CommitInfo, error)
}

// Store is the metadata slice the webhook needs: repo->site resolution and the
// best-effort audit append. *db.Store satisfies it; tests use a fake.
type Store interface {
	// GetSiteByGitHubRepo resolves a live site from its "owner/name" mirror repo.
	GetSiteByGitHubRepo(ctx context.Context, githubRepo *string) (gen.Site, error)
	// InsertAudit appends an audit row (best-effort; never blocks the response).
	InsertAudit(ctx context.Context, arg gen.InsertAuditParams) error
}

// notFound reports whether err is the store's no-rows signal. Injected so the
// webhook package does not depend on db.IsNotFound directly (decoupling); the
// composition root passes db.IsNotFound.
type notFound func(error) bool

// Deps is the dependency bundle for the webhook handler.
type Deps struct {
	// Secret is the KOTOJI_GITHUB_WEBHOOK_SECRET HMAC key. REQUIRED (an empty
	// secret makes New return a handler that rejects every delivery with 503, so a
	// misconfigured instance fails closed rather than accepting unsigned pushes).
	Secret string
	// Site is the fast-forward entry point.
	Site Service
	// Store resolves repo->site and appends audit rows.
	Store Store
	// IsNotFound maps a store miss to "unknown repo" (200/ignored). REQUIRED.
	IsNotFound notFound
	// Logger is used for best-effort warnings (audit failure, FF rejection).
	Logger *slog.Logger
}

// Handler is the GitHub webhook HTTP handler. It is safe for concurrent use.
type Handler struct {
	deps Deps
}

// New builds the webhook Handler. A nil Logger is tolerated (logging is skipped).
func New(deps Deps) *Handler {
	return &Handler{deps: deps}
}

// pushEvent is the minimal slice of the GitHub push payload we read. The full
// payload is large; we decode only what we need (the pushed ref + repo full name).
type pushEvent struct {
	Ref        string `json:"ref"` // "refs/heads/<branch>"
	Repository struct {
		FullName string `json:"full_name"` // "owner/name"
	} `json:"repository"`
}

// ServeHTTP processes one webhook delivery. The flow is fail-closed on auth and
// fail-open (200, ignored) on every "not for us" case so GitHub stops retrying.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// A missing secret is a misconfiguration: fail closed (never accept unsigned).
	if h.deps.Secret == "" {
		http.Error(w, "webhook not configured", http.StatusServiceUnavailable)
		return
	}

	// Read the body under a hard cap BEFORE verifying — we must hash the exact
	// bytes GitHub signed, and the cap bounds the read of an untrusted sender.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	// 1. Verify HMAC-SHA256 in constant time. Reject (401) on any failure: the
	//    body stays untrusted until this passes (architecture.md §8.1.12).
	if !h.verifySignature(r.Header.Get(signatureHeader), body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// 2. Only push events do work; everything else (ping, etc.) is a 200 no-op.
	if !strings.EqualFold(r.Header.Get(eventHeader), "push") {
		writeOK(w, "ignored: not a push event")
		return
	}

	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		// A signed-but-malformed push body is a client error; 400 (not 200) so the
		// sender notices, but we never act on it.
		http.Error(w, "malformed push payload", http.StatusBadRequest)
		return
	}

	repo := strings.TrimSpace(ev.Repository.FullName)
	branch := strings.TrimPrefix(ev.Ref, refPrefix)
	if repo == "" || branch == ev.Ref || branch == "" {
		// No repo, or a non-branch ref (tag push, etc.): nothing to track.
		writeOK(w, "ignored: no trackable branch ref")
		return
	}

	// 3. Map repo -> site. An unknown repo is ignored (200) so kotoji can share a
	//    webhook URL with repos it does not mirror.
	st, err := h.deps.Store.GetSiteByGitHubRepo(r.Context(), &repo)
	if err != nil {
		if h.deps.IsNotFound != nil && h.deps.IsNotFound(err) {
			writeOK(w, "ignored: unknown repo")
			return
		}
		h.warn(r.Context(), "webhook repo lookup failed", "repo", repo, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// 4. Only act on a push to the site's published-tracking branch. A push to any
	//    other branch (draft, feature-*) is recorded-by-omission and ignored: the
	//    webhook's job is to advance published when a PR merges on GitHub
	//    (architecture.md §3f). The tracked branch is the site's published branch.
	tracked := trackedBranch(st)
	if branch != string(tracked) {
		writeOK(w, "ignored: branch not tracked")
		return
	}

	// 5. Fast-forward the local mirror (also refreshes served tree + published
	//    pointer for the published branch — site.FetchAndUpdate owns that).
	ci, err := h.deps.Site.FetchAndUpdate(r.Context(), st.ID, tracked)
	if err != nil {
		// A non-FF push is flagged, never force-applied (architecture.md §8.2.4).
		// We surface it as 200 with a body note so GitHub does not hammer retries;
		// the divergence needs operator action, not a redelivery.
		if errors.Is(err, site.ErrPublishConflict) {
			h.warn(r.Context(), "webhook non-fast-forward; manual sync required", "site", st.ID, "branch", branch)
			h.audit(r.Context(), st.ID, repo, branch, "", "non_fast_forward")
			writeOK(w, "ignored: non-fast-forward (manual sync required)")
			return
		}
		h.warn(r.Context(), "webhook fetch+update failed", "site", st.ID, "branch", branch, "err", err)
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	// 6. Audit the system-sourced update (architecture.md §3f: via=webhook -> system).
	h.audit(r.Context(), st.ID, repo, branch, ci.SHA, "fast_forward")
	writeOK(w, "updated")
}

// verifySignature compares the provided X-Hub-Signature-256 header against the
// HMAC-SHA256 of body under the secret, in constant time. It returns false for a
// missing/malformed header or any mismatch (fail-closed).
func (h *Handler) verifySignature(header string, body []byte) bool {
	if !strings.HasPrefix(header, sigPrefix) {
		return false
	}
	got, err := hex.DecodeString(header[len(sigPrefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.deps.Secret))
	mac.Write(body)
	want := mac.Sum(nil)
	// subtle.ConstantTimeCompare is length-safe and timing-safe.
	return subtle.ConstantTimeCompare(got, want) == 1
}

// trackedBranch is the branch a webhook advances for a site: its published
// branch. The published branch is "published" in v1 (the served-in-prod branch,
// CANONICAL §2); pushes to it (PR merges on GitHub) update the live site.
func trackedBranch(st gen.Site) site.BranchName {
	return site.BranchPublished
}

// audit appends a best-effort, system-sourced audit row for the update. Failure
// is logged, never fatal (audit is observability, off the critical path).
func (h *Handler) audit(ctx context.Context, siteID uuid.UUID, repo, branch, commitSHA, kind string) {
	id := siteID
	var sha *string
	if commitSHA != "" {
		sha = &commitSHA
	}
	arg := gen.InsertAuditParams{
		SiteID:    &id,
		Action:    "webhook.push",
		Source:    gen.AuditSourceSystem, // architecture.md §8: via webhook -> source system
		CommitSha: sha,
		Metadata:  auditMeta(map[string]any{"repo": repo, "branch": branch, "kind": kind}),
	}
	if err := h.deps.Store.InsertAudit(ctx, arg); err != nil {
		h.warn(ctx, "webhook audit insert failed", "site", siteID, "err", err)
	}
}

// auditMeta marshals an audit metadata map to JSONB bytes; a marshal failure
// (impossible for plain maps) degrades to an empty object so audit never blocks.
func auditMeta(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// warn logs at warn level when a logger is wired (no-op otherwise).
func (h *Handler) warn(ctx context.Context, msg string, args ...any) {
	if h.deps.Logger != nil {
		h.deps.Logger.WarnContext(ctx, msg, args...)
	}
}

// writeOK writes a 200 with a short text note (the body is informational; GitHub
// only checks the status). 2xx tells GitHub the delivery was accepted.
func writeOK(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, msg)
}
