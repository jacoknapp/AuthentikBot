package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

type Config struct {
	DiscordToken       string
	AuthentikURL       string
	AuthentikToken     string
	AuthentikFlowSlug  string
	DefaultExpiration  int64    // in hours
	ImmutableUsernames []string // users that cannot be added/removed from groups
}

type InviteRequest struct {
	Name      string                 `json:"name"`                 // Invitation name/slug
	Expires   string                 `json:"expires"`              // ISO 8601 datetime string
	SingleUse bool                   `json:"single_use"`           // Single-use invitation
	FixedData map[string]interface{} `json:"fixed_data,omitempty"` // Optional pre-fill data
}

type InviteResponse struct {
	Pk        string `json:"pk"`         // UUID of the invitation (the itoken parameter)
	Name      string `json:"name"`       // Invitation name
	Expires   string `json:"expires"`    // Expiration datetime
	SingleUse bool   `json:"single_use"` // Whether single-use
	CreatedBy struct {
		Pk       int64  `json:"pk"`
		Username string `json:"username"`
	} `json:"created_by"` // User who created this invitation
}

type User struct {
	Pk       int64  `json:"pk"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Name     string `json:"name"`
}

type UsersResponse struct {
	Results []User `json:"results"`
	Next    string `json:"next"`
	Error   string `json:"error"`
}

type Group struct {
	Pk       int64   `json:"pk"`
	Name     string  `json:"name"`
	Members  []int64 `json:"members"`
	ParentPk *int64  `json:"parent"`
}

type GroupsResponse struct {
	Results []Group `json:"results"`
	Error   string  `json:"error"`
}

var config Config

func init() {
	config.DiscordToken = os.Getenv("DISCORD_TOKEN")
	config.AuthentikURL = os.Getenv("AUTHENTIK_URL")
	config.AuthentikToken = os.Getenv("AUTHENTIK_TOKEN")
	config.AuthentikFlowSlug = os.Getenv("AUTHENTIK_FLOW_SLUG")

	// Parse immutable usernames (comma-separated)
	immutableStr := os.Getenv("IMMUTABLE_USERNAMES")
	if immutableStr != "" {
		config.ImmutableUsernames = strings.Split(immutableStr, ",")
		for i, u := range config.ImmutableUsernames {
			config.ImmutableUsernames[i] = strings.TrimSpace(u)
		}
	}

	defaultExp := os.Getenv("INVITE_EXPIRATION_HOURS")
	if defaultExp == "" {
		config.DefaultExpiration = 24
	} else {
		exp, err := strconv.ParseInt(defaultExp, 10, 64)
		if err != nil {
			log.Fatal("Invalid INVITE_EXPIRATION_HOURS value")
		}
		config.DefaultExpiration = exp
	}

	if config.DiscordToken == "" {
		log.Fatal("Missing required environment variable: DISCORD_TOKEN")
	}
	if config.AuthentikURL == "" {
		log.Fatal("Missing required environment variable: AUTHENTIK_URL")
	}
	if config.AuthentikToken == "" {
		log.Fatal("Missing required environment variable: AUTHENTIK_TOKEN")
	}
	if config.AuthentikFlowSlug == "" {
		log.Fatal("Missing required environment variable: AUTHENTIK_FLOW_SLUG")
	}
}

func main() {
	dg, err := discordgo.New("Bot " + config.DiscordToken)
	if err != nil {
		log.Fatal("Error creating Discord session:", err)
	}

	dg.AddHandler(ready)
	dg.AddHandler(interactionCreate)

	err = dg.Open()
	if err != nil {
		log.Fatal("Error opening Discord session:", err)
	}

	// Register slash command
	registerCommands(dg)

	defer dg.Close()

	log.Println("Bot is running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc
}

func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Logged in as: %v#%v", event.User.Username, event.User.Discriminator)
}

func registerCommands(s *discordgo.Session) {
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "invite",
			Description: "Generate an Authentik invite link",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "expiration",
					Description: "Expiration time in hours (default: " + fmt.Sprintf("%d", config.DefaultExpiration) + ", max: 8760)",
					Required:    false,
				},
			},
		},
		{
			Name:        "group",
			Description: "Manage group membership",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "action",
					Description: "Add or remove user from group",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{
							Name:  "add",
							Value: "add",
						},
						{
							Name:  "remove",
							Value: "remove",
						},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "group",
					Description: "Group name",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "username",
					Description: "Username to add or remove",
					Required:    true,
				},
			},
		},
		{
			Name:        "user",
			Description: "User operations (list)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "action",
					Description: "Action to perform",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "list", Value: "list"},
					},
				},
			},
		},
	}

	registeredCommands, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, "", commands)
	if err != nil {
		log.Printf("Error registering commands: %v", err)
		return
	}

	log.Printf("Successfully registered %d commands", len(registeredCommands))
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	if i.ApplicationCommandData().Name == "invite" {
		handleInviteCommand(s, i)
	} else if i.ApplicationCommandData().Name == "group" {
		handleGroupCommand(s, i)
	} else if i.ApplicationCommandData().Name == "user" {
		handleUserCommand(s, i)
	}
}

func handleInviteCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Defer the response
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	expiration := config.DefaultExpiration
	if len(i.ApplicationCommandData().Options) > 0 {
		for _, opt := range i.ApplicationCommandData().Options {
			if opt.Name == "expiration" {
				expiration = opt.IntValue()
				break
			}
		}
	}

	inviteLink, err := generateAuthentikInvite(expiration)
	if err != nil {
		log.Printf("Error generating invite: %v", err)
		msg, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ Failed to generate invite: " + err.Error()),
		})
		if msg != nil {
			go deleteMessageAfterDelay(s, i.ChannelID, msg.ID, 5*time.Minute)
		}
		return
	}

	// Send the invite link in a code block
	message := fmt.Sprintf("✅ Invite generated (expires in %d hours):\n```\n%s\n```", expiration, inviteLink)

	resp, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: stringPtr(message),
	})

	if err != nil {
		log.Printf("Error sending response: %v", err)
		return
	}

	// Schedule deletion of the message after 5 minutes
	go deleteMessageAfterDelay(s, i.ChannelID, resp.ID, 5*time.Minute)
}

func generateAuthentikInvite(expirationHours int64) (string, error) {
	// Create invitation via Authentik API using the stages/invitation/invitations endpoint
	// Users access the invite via: https://auth.example.com/if/flow/{flow_slug}/?itoken={invitation_uuid}

	// Calculate expiration datetime (ISO 8601 format)
	expiresAt := time.Now().Add(time.Duration(expirationHours) * time.Hour)
	expiresStr := expiresAt.Format("2006-01-02T15:04:05Z")

	// Generate random name for the invitation (must be slug format)
	inviteName := "invite-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]

	inviteReq := InviteRequest{
		Name:      inviteName,
		Expires:   expiresStr,
		SingleUse: true, // All invites are single-use
		FixedData: map[string]interface{}{},
	}

	body, err := json.Marshal(inviteReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	// POST to correct Authentik invitations endpoint
	url := fmt.Sprintf("%s/api/v3/stages/invitation/invitations/", strings.TrimRight(config.AuthentikURL, "/"))
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Authentik: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	// Expected status is 201 Created
	if resp.StatusCode != http.StatusCreated {
		log.Printf("Authentik API returned status %d: %s", resp.StatusCode, string(respBody))
		return "", fmt.Errorf("authentik API returned status %d", resp.StatusCode)
	}

	var inviteResp InviteResponse
	err = json.Unmarshal(respBody, &inviteResp)
	if err != nil {
		log.Printf("Failed to parse response JSON: %s", string(respBody))
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	// Construct invitation URL from response
	// Format: https://authentik.example.com/if/flow/{flow-slug}/?itoken={invitation-uuid}
	if inviteResp.Pk == "" {
		log.Printf("Error: API response missing 'pk' (invitation UUID) field")
		return "", fmt.Errorf("authentik response missing invitation pk")
	}

	baseURL := strings.TrimRight(config.AuthentikURL, "/")
	inviteLink := fmt.Sprintf("%s/if/flow/%s/?itoken=%s", baseURL, config.AuthentikFlowSlug, inviteResp.Pk)

	log.Printf("Successfully created invitation with UUID: %s (expires: %s)", inviteResp.Pk, inviteResp.Expires)
	return inviteLink, nil
}

func stringPtr(s string) *string {
	return &s
}

func ptrInt64(i int64) *int64 {
	return &i
}

func deleteMessageAfterDelay(s *discordgo.Session, channelID, messageID string, delay time.Duration) {
	time.Sleep(delay)
	err := s.ChannelMessageDelete(channelID, messageID)
	if err != nil {
		log.Printf("Error deleting message: %v", err)
		return
	}
	log.Printf("Invite message deleted after 5 minutes (message ID: %s)", messageID)
}

func handleGroupCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	var action, groupName, username string
	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "action":
			action = opt.StringValue()
		case "group":
			groupName = opt.StringValue()
		case "username":
			username = opt.StringValue()
		}
	}

	// Check if user is immutable
	for _, immutable := range config.ImmutableUsernames {
		if strings.EqualFold(username, immutable) {
			m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content: stringPtr("❌ User '" + username + "' is immutable and cannot be modified"),
			})
			if m != nil {
				go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
			}
			return
		}
	}

	var result string
	var err error

	if action == "add" {
		result, err = addUserToGroup(username, groupName)
	} else if action == "remove" {
		result, err = removeUserFromGroup(username, groupName)
	} else {
		m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ Invalid action: " + action),
		})
		if m != nil {
			go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
		}
		return
	}

	if err != nil {
		log.Printf("Error modifying group: %v", err)
		m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ Failed to modify group: " + err.Error()),
		})
		if m != nil {
			go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
		}
		return
	}

	message := fmt.Sprintf("✅ %s", result)
	m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: stringPtr(message),
	})
	if m != nil {
		go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
	}
}

func getUser(username string) (*User, error) {
	q := url.Values{}
	q.Set("username", username)
	urlStr := fmt.Sprintf("%s/api/v3/core/users/?%s", strings.TrimRight(config.AuthentikURL, "/"), q.Encode())

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authentik API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var usersResp UsersResponse
	err = json.Unmarshal(respBody, &usersResp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(usersResp.Results) == 0 {
		return nil, fmt.Errorf("user '%s' not found", username)
	}

	return &usersResp.Results[0], nil
}

func getGroup(groupName string) (*Group, error) {
	q := url.Values{}
	q.Set("name", groupName)
	urlStr := fmt.Sprintf("%s/api/v3/core/groups/?%s", strings.TrimRight(config.AuthentikURL, "/"), q.Encode())

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authentik API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var groupsResp GroupsResponse
	err = json.Unmarshal(respBody, &groupsResp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(groupsResp.Results) == 0 {
		return nil, fmt.Errorf("group '%s' not found", groupName)
	}

	return &groupsResp.Results[0], nil
}

func addUserToGroup(username, groupName string) (string, error) {
	user, err := getUser(username)
	if err != nil {
		return "", err
	}

	group, err := getGroup(groupName)
	if err != nil {
		return "", err
	}

	// Check if user is already in group
	for _, memberID := range group.Members {
		if memberID == user.Pk {
			return "", fmt.Errorf("user '%s' is already in group '%s'", username, groupName)
		}
	}

	// Add user to group
	group.Members = append(group.Members, user.Pk)

	body, err := json.Marshal(group)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v3/core/groups/%d/", strings.TrimRight(config.AuthentikURL, "/"), group.Pk)
	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authentik API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Sprintf("User '%s' added to group '%s'", username, groupName), nil
}

func removeUserFromGroup(username, groupName string) (string, error) {
	user, err := getUser(username)
	if err != nil {
		return "", err
	}

	group, err := getGroup(groupName)
	if err != nil {
		return "", err
	}

	// Check if user is in group and remove
	found := false
	newMembers := make([]int64, 0)
	for _, memberID := range group.Members {
		if memberID == user.Pk {
			found = true
		} else {
			newMembers = append(newMembers, memberID)
		}
	}

	if !found {
		return "", fmt.Errorf("user '%s' is not in group '%s'", username, groupName)
	}

	group.Members = newMembers

	body, err := json.Marshal(group)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v3/core/groups/%d/", strings.TrimRight(config.AuthentikURL, "/"), group.Pk)
	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("authentik API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Sprintf("User '%s' removed from group '%s'", username, groupName), nil
}

// --- Users listing command ---
func handleUserCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})

	action := ""
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "action" {
			action = opt.StringValue()
			break
		}
	}

	if action != "list" {
		msg, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ Invalid action; supported: list"),
		})
		if msg != nil {
			go deleteMessageAfterDelay(s, i.ChannelID, msg.ID, 5*time.Minute)
		}
		return
	}

	users, err := getAllUsers()
	if err != nil {
		log.Printf("Error fetching users: %v", err)
		m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ Failed to fetch users: " + err.Error()),
		})
		if m != nil {
			go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
		}
		return
	}

	// Filter out immutable usernames
	immutable := map[string]bool{}
	for _, u := range config.ImmutableUsernames {
		immutable[strings.ToLower(strings.TrimSpace(u))] = true
	}

	filtered := make([]User, 0, len(users))
	for _, u := range users {
		if immutable[strings.ToLower(u.Username)] {
			continue
		}
		filtered = append(filtered, u)
	}

	// Sort by username
	sort.Slice(filtered, func(i, j int) bool {
		return strings.ToLower(filtered[i].Username) < strings.ToLower(filtered[j].Username)
	})

	if len(filtered) == 0 {
		m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("No users to display (after filtering)."),
		})
		if m != nil {
			go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
		}
		return
	}

	pages := buildUserTablePages(filtered)
	// Edit first page
	firstMsg, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: stringPtr(pages[0])})
	if firstMsg != nil {
		go deleteMessageAfterDelay(s, i.ChannelID, firstMsg.ID, 5*time.Minute)
	}
	// Followups for remaining pages
	for p := 1; p < len(pages); p++ {
		fm, _ := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: pages[p]})
		if fm != nil {
			go deleteMessageAfterDelay(s, fm.ChannelID, fm.ID, 5*time.Minute)
		}
	}
}

func getAllUsers() ([]User, error) {
	all := []User{}
	base := strings.TrimRight(config.AuthentikURL, "/")
	url := fmt.Sprintf("%s/api/v3/core/users/?page_size=200", base)

	client := &http.Client{Timeout: 15 * time.Second}
	for {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to send request: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("authentik API returned status %d: %s", resp.StatusCode, string(body))
		}

		var ur UsersResponse
		if err := json.Unmarshal(body, &ur); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}
		all = append(all, ur.Results...)

		if strings.TrimSpace(ur.Next) == "" {
			break
		}
		url = ur.Next
	}
	return all, nil
}

// Build paginated table pages within Discord 2000-char limits
func buildUserTablePages(users []User) []string {
	// Determine display name for each user (prefer Name, fallback to Email)
	type row struct{ Username, FullName string }
	rows := make([]row, 0, len(users))
	for _, u := range users {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			name = u.Email
		}
		rows = append(rows, row{Username: u.Username, FullName: name})
	}

	// Compute column widths with caps
	maxUser := 0
	maxName := 0
	for _, r := range rows {
		if l := len(r.Username); l > maxUser {
			maxUser = l
		}
		if l := len(r.FullName); l > maxName {
			maxName = l
		}
	}
	if maxUser > 28 {
		maxUser = 28
	}
	if maxName > 44 {
		maxName = 44
	}
	if maxUser < 8 {
		maxUser = 8
	}
	if maxName < 8 {
		maxName = 8
	}

	// Pre-build header and separator
	header := fmt.Sprintf("| %-3s | %-*s | %-*s |\n", "#", maxUser, "username", maxName, "full name")
	sep := "|-----+" + strings.Repeat("-", maxUser+2) + "+" + strings.Repeat("-", maxName+2) + "|\n"

	// Build pages by char limit
	pages := []string{}
	pageIdx := 1
	total := len(rows)
	i := 0
	for i < total {
		// Page header line (outside the code block)
		pageHeader := fmt.Sprintf("Users (%d, excluding immutable) — Page %d", total, pageIdx)
		body := header + sep
		count := 0
		for i < total {
			rn := i + 1
			u := ellipsize(rows[i].Username, maxUser)
			n := ellipsize(rows[i].FullName, maxName)
			line := fmt.Sprintf("| %3d | %-*s | %-*s |\n", rn, maxUser, u, maxName, n)
			// Reserve for code block fences and page header
			if len(pageHeader)+len("```\n")+len(body)+len(line)+len("```") > 1900 {
				break
			}
			body += line
			i++
			count++
			// Safety: hard cap rows per page to avoid edge cases
			if count >= 40 {
				break
			}
		}
		pageContent := fmt.Sprintf("%s\n```\n%s```", pageHeader, body)
		pages = append(pages, pageContent)
		pageIdx++
	}
	return pages
}

func ellipsize(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	if max == 2 {
		return s[:2]
	}
	return s[:max-1] + "…"
}
