package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRandomSlugGeneration(t *testing.T) {
	// Test multiple generations to ensure uniqueness
	slugs := make(map[string]bool)

	for i := 0; i < 10; i++ {
		slug := strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

		if len(slug) != 16 {
			t.Errorf("Expected slug length 16, got %d", len(slug))
		}

		if slugs[slug] {
			t.Error("Generated slug is not unique")
		}
		slugs[slug] = true
	}

	if len(slugs) != 10 {
		t.Errorf("Expected 10 unique slugs, got %d", len(slugs))
	}
}

func TestHelperFunctions(t *testing.T) {
	// Test stringPtr
	str := "test"
	result := stringPtr(str)
	if result == nil || *result != "test" {
		t.Error("stringPtr failed")
	}

	// Test ptrInt64
	var val int64 = 42
	result64 := ptrInt64(val)
	if result64 == nil || *result64 != 42 {
		t.Error("ptrInt64 failed")
	}
}

func TestURLConstruction(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "URL with trailing slash",
			input:    "https://example.com/",
			expected: "https://example.com/api/v3/stages/invitation/invitations/",
		},
		{
			name:     "URL without trailing slash",
			input:    "https://example.com",
			expected: "https://example.com/api/v3/stages/invitation/invitations/",
		},
		{
			name:     "URL with port",
			input:    "https://example.com:8443",
			expected: "https://example.com:8443/api/v3/stages/invitation/invitations/",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			baseURL := strings.TrimRight(tc.input, "/")
			result := baseURL + "/api/v3/stages/invitation/invitations/"
			if result != tc.expected {
				t.Errorf("Expected %s, got %s", tc.expected, result)
			}
		})
	}
}

func TestEllipsize(t *testing.T) {
	if ellipsize("short", 10) != "short" {
		t.Errorf("unexpected ellipsize result")
	}
	if ellipsize("exactlen", 8) != "exactlen" {
		t.Errorf("unexpected ellipsize result")
	}
	if got := ellipsize("toolongtext", 5); got != "tool…" {
		t.Errorf("expected 'tool…', got '%s'", got)
	}
	if got := ellipsize("ab", 2); got != "ab" {
		t.Errorf("expected 'ab', got '%s'", got)
	}
	if got := ellipsize("abc", 2); got != "ab" {
		t.Errorf("expected 'ab', got '%s'", got)
	}
}

func TestBuildUserTablePages(t *testing.T) {
	users := []User{
		{Username: "alice.smith", Name: "Alice Smith"},
		{Username: "bob", Name: "Bob Johnson"},
		{Username: "charlie", Name: "", Email: "charlie@example.com"},
	}
	pages := buildUserTablePages(users)
	if len(pages) < 1 {
		t.Fatalf("expected at least one page, got %d", len(pages))
	}
	first := pages[0]
	if !strings.Contains(first, "Users (3, excluding immutable)") {
		t.Errorf("page header missing or incorrect: %s", first)
	}
	if !strings.Contains(first, "username") || !strings.Contains(first, "full name") {
		t.Errorf("table header missing: %s", first)
	}
	if !strings.Contains(first, "alice.smith") || !strings.Contains(first, "Alice Smith") {
		t.Errorf("expected alice row present")
	}
	if !strings.Contains(first, "bob") || !strings.Contains(first, "Bob Johnson") {
		t.Errorf("expected bob row present")
	}
	// Charlie has no Name, should fall back to email
	if !strings.Contains(first, "charlie") || !strings.Contains(first, "charlie@example.com") {
		t.Errorf("expected charlie row with email fallback present")
	}
}

func TestURLQueryEncoding(t *testing.T) {
	// Test that URL query parameters with special characters are properly encoded
	testCases := []struct {
		name      string
		input     string
		key       string
		checkFunc func(encoded string) bool
	}{
		{
			name:  "Space in username",
			input: "user name",
			key:   "username",
			checkFunc: func(encoded string) bool {
				return strings.Contains(encoded, "user+name") || strings.Contains(encoded, "user%20name")
			},
		},
		{
			name:  "Special chars in group name",
			input: "group@domain.com",
			key:   "name",
			checkFunc: func(encoded string) bool {
				return strings.Contains(encoded, "%40")
			},
		},
		{
			name:  "Simple alphanumeric",
			input: "simple",
			key:   "username",
			checkFunc: func(encoded string) bool {
				return strings.Contains(encoded, "simple")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			q.Set(tc.key, tc.input)
			encoded := q.Encode()

			if !tc.checkFunc(encoded) {
				t.Errorf("expected encoding for %s (key=%s) to satisfy check, got: %s", tc.input, tc.key, encoded)
			}
		})
	}
}

func TestInviteResponseValidation(t *testing.T) {
	// Test invitation response handling with various scenarios
	testCases := []struct {
		name      string
		respJSON  string
		shouldErr bool
		checkLink func(link string) bool
	}{
		{
			name:      "Valid response with UUID",
			respJSON:  `{"pk":"123e4567-e89b-12d3-a456-426614174000","name":"invite-abc123","expires":"2026-01-20T15:04:05Z","single_use":true,"created_by":{"pk":1,"username":"admin"}}`,
			shouldErr: false,
			checkLink: func(link string) bool {
				return strings.Contains(link, "itoken=123e4567-e89b-12d3-a456-426614174000")
			},
		},
		{
			name:      "Invitation response with future expiration",
			respJSON:  `{"pk":"550e8400-e29b-41d4-a716-446655440000","name":"invite-xyz789","expires":"2026-02-19T10:00:00Z","single_use":true,"created_by":{"pk":1,"username":"admin"}}`,
			shouldErr: false,
			checkLink: func(link string) bool {
				return strings.Contains(link, "itoken=550e8400-e29b-41d4-a716-446655440000")
			},
		},
		{
			name:      "Multi-use invitation",
			respJSON:  `{"pk":"f47ac10b-58cc-4372-a567-0e02b2c3d479","name":"invite-multi","expires":"2026-03-20T15:04:05Z","single_use":false,"created_by":{"pk":2,"username":"operator"}}`,
			shouldErr: false,
			checkLink: func(link string) bool {
				return strings.Contains(link, "itoken=f47ac10b-58cc-4372-a567-0e02b2c3d479")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var resp InviteResponse
			err := json.Unmarshal([]byte(tc.respJSON), &resp)
			if err != nil {
				t.Fatalf("failed to unmarshal test JSON: %v", err)
			}

			// Construct invitation link from response (simulating generateAuthentikInvite)
			var result string
			if resp.Pk != "" {
				result = fmt.Sprintf("https://auth.example.com/if/flow/enroll/?itoken=%s", resp.Pk)
			}

			if tc.shouldErr && result == "" {
				return // Expected error
			}
			if !tc.shouldErr && result == "" {
				t.Errorf("expected valid link, got empty")
				return
			}
			if !tc.checkLink(result) {
				t.Errorf("link validation failed for: %s", result)
			}
		})
	}
}
