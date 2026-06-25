package site

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValidatePath covers the traversal/ZipSlip/allowlist gate (CANONICAL §5.4).
func TestValidatePath(t *testing.T) {
	valid := []string{"index.html", "css/app.css", "js/lib/util.js", "img/logo.png", "data.json"}
	for _, p := range valid {
		assert.NoErrorf(t, validatePath(p), "path %q should be valid", p)
	}
	invalid := []string{
		"", "/abs.html", "../escape.html", "a/../../b.html",
		`a\b.html`, ".git/config", "sub/.git/x", "x.php", "run.sh", "bin.exe",
	}
	for _, p := range invalid {
		assert.Truef(t, errors.Is(validatePath(p), ErrValidation), "path %q should be rejected", p)
	}
}

// TestValidateMCPWritePath rejects large media (text-first allowlist, §5.6).
func TestValidateMCPWritePath(t *testing.T) {
	assert.NoError(t, validateMCPWritePath("index.html"))
	assert.NoError(t, validateMCPWritePath("logo.png"))
	for _, p := range []string{"clip.mp4", "song.mp3", "doc.pdf", "v.webm", "a.wav"} {
		assert.Truef(t, errors.Is(validateMCPWritePath(p), ErrValidation), "MCP write of %q should be rejected", p)
	}
}

// TestValidateReadPath allows any extension but still blocks traversal.
func TestValidateReadPath(t *testing.T) {
	assert.NoError(t, validateReadPath("weird.bin")) // reads allow any extension
	assert.True(t, errors.Is(validateReadPath("../x"), ErrValidation))
	assert.True(t, errors.Is(validateReadPath(".git/HEAD"), ErrValidation))
}

// TestValidateDir allows "" (root) and rejects traversal.
func TestValidateDir(t *testing.T) {
	assert.NoError(t, validateDir(""))
	assert.NoError(t, validateDir("css"))
	assert.True(t, errors.Is(validateDir("../up"), ErrValidation))
}

// TestMIMEByExt_SingleSource asserts the table is the upload allowlist source and
// includes the contracted minimum set (CANONICAL §5.6).
func TestMIMEByExt_SingleSource(t *testing.T) {
	required := []string{
		".html", ".htm", ".css", ".js", ".mjs", ".json", ".map", ".svg",
		".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".ico",
		".woff", ".woff2", ".ttf", ".otf", ".txt", ".xml",
		".webmanifest", ".manifest", ".csv", ".wasm",
	}
	for _, ext := range required {
		_, ok := MIMEByExt[ext]
		assert.Truef(t, ok, "MIMEByExt must contain %s", ext)
		assert.Truef(t, extAllowedForUpload("file"+ext), "upload allowlist must include %s", ext)
	}
	// Upload allowlist == keys(MIMEByExt).
	assert.False(t, extAllowedForUpload("x.php"))
	assert.Equal(t, "text/html; charset=utf-8", ContentTypeFor("index.html"))
	assert.Equal(t, "", ContentTypeFor("x.php"))
}

// TestContentTypeFor_CaseInsensitive checks extension lowercasing.
func TestContentTypeFor_CaseInsensitive(t *testing.T) {
	assert.Equal(t, "image/png", ContentTypeFor("LOGO.PNG"))
}

// TestStatusFor exhaustively maps the error taxonomy to HTTP statuses (§3).
func TestStatusFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusOK},
		{ErrNotFound, http.StatusNotFound},
		{ErrForbidden, http.StatusForbidden},
		{ErrConflict, http.StatusConflict},
		{ErrHandleTaken, http.StatusConflict},
		{ErrPublishConflict, http.StatusConflict},
		{ErrBranchExists, http.StatusConflict},
		{ErrNothingToCommit, http.StatusConflict},
		{ErrZipTooLarge, http.StatusRequestEntityTooLarge},
		{ErrZipTooManyFiles, http.StatusRequestEntityTooLarge},
		{ErrQuotaExceeded, http.StatusRequestEntityTooLarge},
		{ErrZipBadType, http.StatusUnsupportedMediaType},
		{ErrZipSlip, http.StatusBadRequest},
		{ErrValidation, http.StatusUnprocessableEntity},
		{ErrReservedHandle, http.StatusUnprocessableEntity},
		{ErrGit, http.StatusInternalServerError},
		{errors.New("unknown"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		assert.Equalf(t, c.want, statusFor(c.err), "status for %v", c.err)
	}
}

// TestTypedErrors_WrapSentinels asserts each typed error reports its sentinel via
// errors.Is and exposes its structured fields via errors.As (§3).
func TestTypedErrors_WrapSentinels(t *testing.T) {
	ve := &ValidationError{Field: "path", Reason: "bad"}
	assert.True(t, errors.Is(ve, ErrValidation))
	var gotVE *ValidationError
	assert.True(t, errors.As(ve, &gotVE))
	assert.Equal(t, "path", gotVE.Field)

	ce := &ConflictError{Branch: "draft", Expected: "a", Actual: "b", ChangedPaths: []string{"x.html"}}
	assert.True(t, errors.Is(ce, ErrConflict))
	var gotCE *ConflictError
	assert.True(t, errors.As(ce, &gotCE))
	assert.Equal(t, []string{"x.html"}, gotCE.ChangedPaths)

	pe := &PublishConflictError{Paths: []string{"a", "b"}}
	assert.True(t, errors.Is(pe, ErrPublishConflict))

	ge := &GitError{Args: []string{"commit"}, ExitCode: 1, Stderr: "boom"}
	assert.True(t, errors.Is(ge, ErrGit))
	var gotGE *GitError
	assert.True(t, errors.As(ge, &gotGE))
	assert.Equal(t, 1, gotGE.ExitCode)
}

// TestParseHostOrPath covers the resolver host/path split (CANONICAL §5.3).
func TestParseHostOrPath(t *testing.T) {
	tests := []struct {
		in     string
		handle Handle
		branch BranchName
		err    bool
	}{
		{"site.hosting.example.com", "site", BranchPublished, false},
		{"site--draft.hosting.example.com", "site", "draft", false},
		{"site.localhost:8080", "site", BranchPublished, false},
		{"/host/site/index.html", "site", BranchPublished, false},
		{"/host/site--draft/app.js", "site", "draft", false},
		{"", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			h, b, err := parseHostOrPath(tt.in)
			if tt.err {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.handle, h)
			assert.Equal(t, tt.branch, b)
		})
	}
}
