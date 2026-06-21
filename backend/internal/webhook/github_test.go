package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/necorox-com/kotoji/backend/internal/db/gen"
	"github.com/necorox-com/kotoji/backend/internal/site"
)

const testSecret = "hunter2-webhook-secret"

// errNoRows is the fake store's no-rows sentinel; the handler is given a matching
// IsNotFound so an unknown repo is ignored (200).
var errNoRows = errors.New("no rows")

// fakeStore implements webhook.Store: repo->site mapping + audit capture.
type fakeStore struct {
	byRepo map[string]gen.Site
	audits []gen.InsertAuditParams
}

func newFakeStore() *fakeStore { return &fakeStore{byRepo: map[string]gen.Site{}} }

func (s *fakeStore) GetSiteByGitHubRepo(_ context.Context, repo *string) (gen.Site, error) {
	if repo == nil {
		return gen.Site{}, errNoRows
	}
	st, ok := s.byRepo[*repo]
	if !ok {
		return gen.Site{}, errNoRows
	}
	return st, nil
}

func (s *fakeStore) InsertAudit(_ context.Context, arg gen.InsertAuditParams) error {
	s.audits = append(s.audits, arg)
	return nil
}

// recordingSvc wraps a real-ish FetchAndUpdate seam, recording invocations.
type recordingSvc struct {
	called  int
	lastID  uuid.UUID
	lastBr  site.BranchName
	retErr  error
	retInfo site.CommitInfo
}

func (r *recordingSvc) FetchAndUpdate(_ context.Context, id uuid.UUID, branch site.BranchName) (site.CommitInfo, error) {
	r.called++
	r.lastID = id
	r.lastBr = branch
	return r.retInfo, r.retErr
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return sigPrefix + hex.EncodeToString(m.Sum(nil))
}

func pushBody(t *testing.T, ref, repo string) []byte {
	t.Helper()
	ev := map[string]any{
		"ref":        ref,
		"repository": map[string]any{"full_name": repo},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newHandler(store Store, svc Service) *Handler {
	return New(Deps{
		Secret:     testSecret,
		Site:       svc,
		Store:      store,
		IsNotFound: func(err error) bool { return errors.Is(err, errNoRows) },
	})
}

// doPush posts a (optionally signed) push payload and returns the recorder.
func doPush(h *Handler, event, signature string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", strings.NewReader(string(body)))
	r.Header.Set(eventHeader, event)
	if signature != "" {
		r.Header.Set(signatureHeader, signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestWebhook_ValidSignaturePushUpdates(t *testing.T) {
	store := newFakeStore()
	svc := &recordingSvc{retInfo: site.CommitInfo{SHA: "abc123"}}
	siteID := uuid.New()
	store.byRepo["owner/repo"] = gen.Site{ID: siteID, GithubRepo: ptr("owner/repo")}

	h := newHandler(store, svc)
	body := pushBody(t, "refs/heads/published", "owner/repo")
	rec := doPush(h, "push", sign(testSecret, body), body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if svc.called != 1 {
		t.Fatalf("FetchAndUpdate called %d times want 1", svc.called)
	}
	if svc.lastID != siteID || svc.lastBr != site.BranchPublished {
		t.Fatalf("FetchAndUpdate(%v,%v) want (%v, published)", svc.lastID, svc.lastBr, siteID)
	}
	if len(store.audits) != 1 {
		t.Fatalf("audits = %d want 1", len(store.audits))
	}
	if store.audits[0].Source != gen.AuditSourceSystem {
		t.Fatalf("audit source = %v want system", store.audits[0].Source)
	}
}

func TestWebhook_BadSignatureRejected(t *testing.T) {
	store := newFakeStore()
	svc := &recordingSvc{}
	store.byRepo["owner/repo"] = gen.Site{ID: uuid.New(), GithubRepo: ptr("owner/repo")}
	h := newHandler(store, svc)
	body := pushBody(t, "refs/heads/published", "owner/repo")

	// Sign with the WRONG secret.
	rec := doPush(h, "push", sign("wrong-secret", body), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401", rec.Code)
	}
	if svc.called != 0 {
		t.Fatalf("FetchAndUpdate must not run on a bad signature")
	}

	// Missing signature header entirely.
	rec2 := doPush(h, "push", "", body)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("missing-sig status = %d want 401", rec2.Code)
	}
}

func TestWebhook_NonTrackedBranchNoOp(t *testing.T) {
	store := newFakeStore()
	svc := &recordingSvc{}
	store.byRepo["owner/repo"] = gen.Site{ID: uuid.New(), GithubRepo: ptr("owner/repo")}
	h := newHandler(store, svc)

	// Push to draft (not the published-tracking branch).
	body := pushBody(t, "refs/heads/draft", "owner/repo")
	rec := doPush(h, "push", sign(testSecret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (ignored)", rec.Code)
	}
	if svc.called != 0 {
		t.Fatalf("FetchAndUpdate must not run for a non-tracked branch")
	}
}

func TestWebhook_UnknownRepoIgnored(t *testing.T) {
	store := newFakeStore() // empty: no repo maps
	svc := &recordingSvc{}
	h := newHandler(store, svc)

	body := pushBody(t, "refs/heads/published", "stranger/repo")
	rec := doPush(h, "push", sign(testSecret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (ignored unknown repo)", rec.Code)
	}
	if svc.called != 0 {
		t.Fatalf("FetchAndUpdate must not run for an unknown repo")
	}
}

func TestWebhook_NonPushEventIgnored(t *testing.T) {
	store := newFakeStore()
	svc := &recordingSvc{}
	h := newHandler(store, svc)
	body := []byte(`{"zen":"ping"}`)
	rec := doPush(h, "ping", sign(testSecret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("ping status = %d want 200", rec.Code)
	}
	if svc.called != 0 {
		t.Fatalf("non-push event must be a no-op")
	}
}

func TestWebhook_NonFastForwardFlagged(t *testing.T) {
	store := newFakeStore()
	svc := &recordingSvc{retErr: site.ErrPublishConflict}
	store.byRepo["owner/repo"] = gen.Site{ID: uuid.New(), GithubRepo: ptr("owner/repo")}
	h := newHandler(store, svc)

	body := pushBody(t, "refs/heads/published", "owner/repo")
	rec := doPush(h, "push", sign(testSecret, body), body)
	// A non-FF is flagged + 200 (so GitHub does not retry), not a 5xx.
	if rec.Code != http.StatusOK {
		t.Fatalf("non-FF status = %d want 200 (flagged)", rec.Code)
	}
	if len(store.audits) != 1 || store.audits[0].Action != "webhook.push" {
		t.Fatalf("non-FF should still audit; got %+v", store.audits)
	}
}

func TestWebhook_MissingSecretFailsClosed(t *testing.T) {
	store := newFakeStore()
	svc := &recordingSvc{}
	h := New(Deps{Secret: "", Site: svc, Store: store, IsNotFound: func(error) bool { return false }})
	body := pushBody(t, "refs/heads/published", "owner/repo")
	rec := doPush(h, "push", sign("anything", body), body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing-secret status = %d want 503", rec.Code)
	}
}

func TestWebhook_RealFakeServiceEndToEnd(t *testing.T) {
	// Exercise the FetchAndUpdate path against the real site.FakeService (contract
	// parity) rather than a recorder: it returns the current tip with no error.
	svc := site.NewFakeService()
	st, err := svc.CreateSite(context.Background(), site.CreateSiteInput{
		Handle:  site.Handle("webhook-site"),
		OwnerID: uuid.New(),
		Actor:   site.Actor{UserID: uuid.New(), Via: site.SourceSystem},
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	// Publish so a "published" branch exists for FetchAndUpdate to resolve.
	branches, _ := svc.ListBranches(context.Background(), st.ID)
	var draftTip string
	for _, b := range branches {
		if b.Name == site.BranchDraft {
			draftTip = b.HeadSHA
		}
	}
	if _, err := svc.Publish(context.Background(), site.PublishInput{SiteID: st.ID, From: site.BranchDraft, BaseSHA: draftTip, Actor: site.Actor{UserID: uuid.New(), Via: site.SourceSystem}}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	store := newFakeStore()
	store.byRepo["owner/webhook-site"] = gen.Site{ID: st.ID, GithubRepo: ptr("owner/webhook-site")}
	h := newHandler(store, svc)

	body := pushBody(t, "refs/heads/published", "owner/webhook-site")
	rec := doPush(h, "push", sign(testSecret, body), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func ptr[T any](v T) *T { return &v }
