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

func TestIsCommandAllowed(t *testing.T) {
	// Save original config
	origChannelID := config.CommandChannelID
	defer func() { config.CommandChannelID = origChannelID }()

	testCases := []struct {
		name            string
		configChannelID string
		testChannelID   string
		expectedAllowed bool
	}{
		{
			name:            "No restriction - should allow",
			configChannelID: "",
			testChannelID:   "123456789",
			expectedAllowed: true,
		},
		{
			name:            "Matching channel - should allow",
			configChannelID: "987654321",
			testChannelID:   "987654321",
			expectedAllowed: true,
		},
		{
			name:            "Different channel - should deny",
			configChannelID: "987654321",
			testChannelID:   "123456789",
			expectedAllowed: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config.CommandChannelID = tc.configChannelID
			result := isCommandAllowed(tc.testChannelID)
			if result != tc.expectedAllowed {
				t.Errorf("expected %v, got %v", tc.expectedAllowed, result)
			}
		})
	}
}

func TestBuildGroupTablePages(t *testing.T) {
	groups := []Group{
		{Pk: "1", Name: "developers", Members: []int64{1, 2, 3}},
		{Pk: "2", Name: "admins", Members: []int64{4}},
		{Pk: "3", Name: "support", Members: []int64{}},
	}

	pages := buildGroupTablePages(groups)
	if len(pages) < 1 {
		t.Fatalf("expected at least one page, got %d", len(pages))
	}

	first := pages[0]
	if !strings.Contains(first, "Groups (3 total)") {
		t.Errorf("page header missing or incorrect: %s", first)
	}
	if !strings.Contains(first, "group name") || !strings.Contains(first, "members") {
		t.Errorf("table header missing: %s", first)
	}
	if !strings.Contains(first, "developers") || !strings.Contains(first, "3") {
		t.Errorf("expected developers row with 3 members")
	}
	if !strings.Contains(first, "admins") || !strings.Contains(first, "1") {
		t.Errorf("expected admins row with 1 member")
	}
	if !strings.Contains(first, "support") || !strings.Contains(first, "0") {
		t.Errorf("expected support row with 0 members")
	}
}

func TestGroupAndUserStructs(t *testing.T) {
	// Test Group struct unmarshaling
	groupJSON := `{"pk":"123","name":"test-group","users":[1,2,3],"parent":null}`
	var group Group
	if err := json.Unmarshal([]byte(groupJSON), &group); err != nil {
		t.Errorf("failed to unmarshal group: %v", err)
	}
	if group.Pk != "123" || group.Name != "test-group" || len(group.Members) != 3 {
		t.Errorf("group unmarshaling incorrect: %+v", group)
	}

	// Test User struct unmarshaling
	userJSON := `{"pk":"456","num_pk":42,"username":"testuser","email":"test@example.com","name":"Test User"}`
	var user User
	if err := json.Unmarshal([]byte(userJSON), &user); err != nil {
		t.Errorf("failed to unmarshal user: %v", err)
	}
	if user.Pk != "456" || user.NumPk != 42 || user.Username != "testuser" || user.Email != "test@example.com" {
		t.Errorf("user unmarshaling incorrect: %+v", user)
	}
}

func TestInviteRequestStructure(t *testing.T) {
	req := InviteRequest{
		Name:      "test-invite",
		Expires:   "2026-12-31T23:59:59Z",
		SingleUse: true,
		FixedData: map[string]interface{}{
			"email": "test@example.com",
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal invite request: %v", err)
	}

	var decoded InviteRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal invite request: %v", err)
	}

	if decoded.Name != "test-invite" || !decoded.SingleUse {
		t.Errorf("invite request roundtrip failed: %+v", decoded)
	}
}

func TestEllipsizeEdgeCases(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		max      int
		expected string
	}{
		{"Empty string", "", 10, ""},
		{"Exact length", "exactly10c", 10, "exactly10c"},
		{"One char max", "test", 1, "t"},
		{"Two char max", "test", 2, "te"},
		{"Three char max with ellipsis", "test", 3, "te…"},
		{"Long string truncated", "this is a very long string", 10, "this is a…"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := ellipsize(tc.input, tc.max)
			if result != tc.expected {
				t.Errorf("expected '%s', got '%s'", tc.expected, result)
			}
		})
	}
}

func TestUsersResponseStructure(t *testing.T) {
	respJSON := `{
		"results": [
			{"pk":"uuid-1","num_pk":1,"username":"user1","email":"user1@example.com","name":"User One"},
			{"pk":"uuid-2","num_pk":2,"username":"user2","email":"user2@example.com","name":"User Two"}
		],
		"next": "https://example.com/api/v3/core/users/?page=2"
	}`

	var resp UsersResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		t.Fatalf("failed to unmarshal users response: %v", err)
	}

	if len(resp.Results) != 2 {
		t.Errorf("expected 2 users, got %d", len(resp.Results))
	}
	if resp.Next != "https://example.com/api/v3/core/users/?page=2" {
		t.Errorf("next URL incorrect: %s", resp.Next)
	}
}

func TestGroupsResponseStructure(t *testing.T) {
	respJSON := `{
		"results": [
			{"pk":"uuid-1","name":"group1","users":[1,2],"parent":null},
			{"pk":"uuid-2","name":"group2","users":[3],"parent":null}
		]
	}`

	var resp GroupsResponse
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		t.Fatalf("failed to unmarshal groups response: %v", err)
	}

	if len(resp.Results) != 2 {
		t.Errorf("expected 2 groups, got %d", len(resp.Results))
	}
	if resp.Results[0].Name != "group1" {
		t.Errorf("first group name incorrect: %s", resp.Results[0].Name)
	}
}

func TestAutocompleteFiltering(t *testing.T) {
	// Test the logic that would be used in autocomplete filtering
	groups := []Group{
		{Name: "developers"},
		{Name: "dev-ops"},
		{Name: "design"},
		{Name: "administrators"},
	}

	testCases := []struct {
		name          string
		input         string
		expectedCount int
		shouldContain string
	}{
		{"Empty input", "", 4, "developers"},
		{"Match 'dev'", "dev", 2, "developers"},
		{"Match 'admin'", "admin", 1, "administrators"},
		{"No match", "xyz", 0, ""},
		{"Case insensitive", "DEV", 2, "developers"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.ToLower(tc.input)
			var matches []string
			for _, g := range groups {
				if input == "" || strings.Contains(strings.ToLower(g.Name), input) {
					matches = append(matches, g.Name)
				}
			}

			if len(matches) != tc.expectedCount {
				t.Errorf("expected %d matches, got %d: %v", tc.expectedCount, len(matches), matches)
			}

			if tc.expectedCount > 0 {
				found := false
				for _, m := range matches {
					if m == tc.shouldContain {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected to find '%s' in matches: %v", tc.shouldContain, matches)
				}
			}
		})
	}
}

func TestImmutableUsernamesFiltering(t *testing.T) {
	// Save and restore original config
	origImmutable := config.ImmutableUsernames
	defer func() { config.ImmutableUsernames = origImmutable }()

	config.ImmutableUsernames = []string{"admin", "root", "system"}

	users := []User{
		{Username: "admin", Name: "Administrator"},
		{Username: "alice", Name: "Alice"},
		{Username: "root", Name: "Root User"},
		{Username: "bob", Name: "Bob"},
	}

	// Build immutable map (like autocomplete would do)
	immutable := map[string]bool{}
	for _, u := range config.ImmutableUsernames {
		immutable[strings.ToLower(strings.TrimSpace(u))] = true
	}

	// Filter users
	var filtered []User
	for _, u := range users {
		if !immutable[strings.ToLower(u.Username)] {
			filtered = append(filtered, u)
		}
	}

	if len(filtered) != 2 {
		t.Errorf("expected 2 filtered users, got %d", len(filtered))
	}

	for _, u := range filtered {
		if u.Username == "admin" || u.Username == "root" {
			t.Errorf("immutable user %s should have been filtered", u.Username)
		}
	}
}

func TestConfigStructFields(t *testing.T) {
	cfg := Config{
		DiscordToken:       "test-token",
		AuthentikURL:       "https://auth.example.com",
		AuthentikToken:     "test-auth-token",
		AuthentikFlowSlug:  "enrollment",
		DefaultExpiration:  24,
		ImmutableUsernames: []string{"admin", "root"},
		CommandChannelID:   "123456",
	}

	if cfg.DiscordToken != "test-token" {
		t.Errorf("DiscordToken field incorrect")
	}
	if cfg.DefaultExpiration != 24 {
		t.Errorf("DefaultExpiration field incorrect")
	}
	if len(cfg.ImmutableUsernames) != 2 {
		t.Errorf("ImmutableUsernames field incorrect")
	}
}
