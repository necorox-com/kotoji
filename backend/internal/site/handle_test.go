package site

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migration0002Path locates the reserved-handle seed relative to this package.
const migration0002Path = "../db/migrations/0002_seed_reserved.sql"

// reservedSeedRe extracts handles from the 0002 INSERT VALUES rows of the form
// ('handle', 'reason') — the same shape the db package's own test uses.
var reservedSeedRe = regexp.MustCompile(`\(\s*'([a-z0-9-]+)'\s*,\s*'[^']*'\s*\)`)

// TestReservedHandles_MatchMigration0002 asserts the Go ReservedHandles slice
// equals EXACTLY the handles seeded by migration 0002 (the dual-maintenance
// guard from CANONICAL §5.1 / §4.1). Drift fails the build.
func TestReservedHandles_MatchMigration0002(t *testing.T) {
	src, err := os.ReadFile(filepath.Clean(migration0002Path))
	require.NoError(t, err, "read migration 0002")

	// Scan only the Up INSERT section so the Down DELETE list does not match.
	upEnd := indexOf(string(src), "-- +goose Down")
	require.GreaterOrEqual(t, upEnd, 0, "migration must have a Down section")
	up := string(src)[:upEnd]

	matches := reservedSeedRe.FindAllStringSubmatch(up, -1)
	require.NotEmpty(t, matches, "no reserved handles parsed from seed INSERT")

	seeded := make([]string, 0, len(matches))
	for _, m := range matches {
		seeded = append(seeded, m[1])
	}
	want := append([]string(nil), ReservedHandles...)
	sort.Strings(want)
	sort.Strings(seeded)
	assert.Equal(t, want, seeded,
		"site.ReservedHandles must match 0002_seed_reserved.sql exactly")
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestValidateHandle covers the create-time grammar (CANONICAL §5.1).
func TestValidateHandle(t *testing.T) {
	tests := []struct {
		name    string
		handle  Handle
		wantErr error // nil | ErrValidation | ErrReservedHandle
	}{
		{"valid simple", "mysite", nil},
		{"valid with hyphen", "my-cool-site", nil},
		{"valid alnum", "tool123", nil},
		{"too short", "ab", ErrValidation},
		{"uppercase", "MySite", ErrValidation},
		{"leading hyphen", "-site", ErrValidation},
		{"trailing hyphen", "site-", ErrValidation},
		{"double hyphen", "my--site", ErrValidation},
		{"underscore", "my_site", ErrValidation},
		{"empty", "", ErrValidation},
		{"reserved draft", "draft", ErrReservedHandle},
		{"reserved api", "api", ErrReservedHandle},
		{"too long", Handle(longString(64)), ErrValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateHandle(tt.handle)
			if tt.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			assert.Truef(t, errors.Is(err, tt.wantErr), "got %v want %v", err, tt.wantErr)
		})
	}
}

// TestValidateHandleForResolver accepts 1-char handles (CANONICAL §5.1).
func TestValidateHandleForResolver(t *testing.T) {
	assert.NoError(t, ValidateHandleForResolver("a"))
	assert.NoError(t, ValidateHandleForResolver("ab"))
	assert.True(t, errors.Is(ValidateHandleForResolver("A"), ErrValidation))
	assert.True(t, errors.Is(ValidateHandleForResolver("a--b"), ErrValidation))
}

// TestSplitLabel covers the "--" separator rule (CANONICAL §5.3).
func TestSplitLabel(t *testing.T) {
	tests := []struct {
		label      string
		wantHandle Handle
		wantBranch BranchName
	}{
		{"expense-calc", "expense-calc", BranchPublished},
		{"expense-calc--draft", "expense-calc", "draft"},
		{"expense-calc--feature-x", "expense-calc", "feature-x"},
		{"a--b--c", "a", "b--c"}, // malformed -> branch fails validation upstream
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			h, b := splitLabel(tt.label)
			assert.Equal(t, tt.wantHandle, h)
			assert.Equal(t, tt.wantBranch, b)
		})
	}
}

// TestPreviewSubdomain covers the host-label fragment (CANONICAL §2).
func TestPreviewSubdomain(t *testing.T) {
	assert.Equal(t, "site", previewSubdomain("site", BranchPublished))
	assert.Equal(t, "site--draft", previewSubdomain("site", BranchDraft))
	assert.Equal(t, "site--feature-x", previewSubdomain("site", "feature-x"))
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
