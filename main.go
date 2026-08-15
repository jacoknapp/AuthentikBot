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
	DefaultExpiration  int64
	ImmutableUsernames []string
	CommandChannelID   string
	RequiredRoleID     string // optional: restrict commands to members with this Discord role ID
}

type InviteRequest struct {
	Name      string                 `json:"name"`
	Expires   string                 `json:"expires"`
	SingleUse bool                   `json:"single_use"`
	FixedData map[string]interface{} `json:"fixed_data,omitempty"`
}

type InviteResponse struct {
	Pk        string `json:"pk"`
	Name      string `json:"name"`
	Expires   string `json:"expires"`
	SingleUse bool   `json:"single_use"`
	CreatedBy struct {
		Pk       int64  `json:"pk"`
		Username string `json:"username"`
	} `json:"created_by"`
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
	Pk       string  `json:"pk"`
	Name     string  `json:"name"`
	Members  []int64 `json:"users"`
	ParentPk *string `json:"parent"`
}

type GroupsResponse struct {
	Results []Group `json:"results"`
	Next    string  `json:"next"`
	Error   string  `json:"error"`
}

var config Config
var apiClient = &http.Client{Timeout: 10 * time.Second}

func init() {
	config.DiscordToken = os.Getenv("DISCORD_TOKEN")
	config.AuthentikURL = os.Getenv("AUTHENTIK_URL")
	config.AuthentikToken = os.Getenv("AUTHENTIK_TOKEN")
	config.AuthentikFlowSlug = os.Getenv("AUTHENTIK_FLOW_SLUG")
	config.CommandChannelID = os.Getenv("COMMAND_CHANNEL_ID")
	config.RequiredRoleID = os.Getenv("REQUIRED_ROLE_ID")

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
	// Required, not optional: the channel lock is the control that keeps
	// /invite and /group off every other channel in the guild.
	if config.CommandChannelID == "" {
		log.Fatal("Missing required environment variable: COMMAND_CHANNEL_ID")
	}
	if config.RequiredRoleID == "" {
		log.Println("⚠️ REQUIRED_ROLE_ID is not set: every member who can post in the designated channel may run privileged commands")
	}
}

func main() {
	dg, err := discordgo.New("Bot " + config.DiscordToken)
	if err != nil {
		log.Fatal("Error creating Discord session:", err)
	}

	dg.AddHandler(ready)
	dg.AddHandler(interactionCreate)

	// Open the gateway with exponential backoff. Never exit-and-let-Docker-restart
	// on a failed connect: each restart is a fresh IDENTIFY, and rapid repeated
	// IDENTIFYs are flagged by Discord as abuse (and get the bot token reset).
	// Once Open succeeds, discordgo handles reconnects internally via RESUME.
	backoff := 5 * time.Second
	const maxBackoff = 15 * time.Minute
	for {
		err = dg.Open()
		if err == nil {
			break
		}
		log.Printf("Error opening Discord session: %v (retrying in %s)", err, backoff)
		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
	defer dg.Close()

	log.Println("Bot is running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM)
	<-sc
}

func ready(s *discordgo.Session, event *discordgo.Ready) {
	log.Printf("Logged in as: %v#%v", event.User.Username, event.User.Discriminator)
	log.Println("🔄 Syncing slash commands...")
	registerCommands(s)
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
					Description: "Action to perform",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "list", Value: "list"},
						{Name: "add", Value: "add"},
						{Name: "remove", Value: "remove"},
					},
				},
				{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "group",
					Description:  "Group name",
					Required:     false,
					Autocomplete: true,
				},
				{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         "username",
					Description:  "Username to add or remove",
					Required:     false,
					Autocomplete: true,
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
		log.Printf("❌ Error registering commands: %v", err)
		return
	}

	log.Printf("✅ Successfully registered %d commands", len(registeredCommands))
	for _, cmd := range registeredCommands {
		log.Printf("   • /%s", cmd.Name)
	}
}

// isCommandAllowed fails closed: an unset COMMAND_CHANNEL_ID denies every
// channel rather than allowing all of them, so a config that fails to load
// locks the bot down instead of opening privileged Authentik commands to the
// whole guild. Startup already refuses to run without the value set.
func isCommandAllowed(channelID string) bool {
	if config.CommandChannelID == "" {
		return false
	}
	return channelID == config.CommandChannelID
}

func isUserAuthorized(member *discordgo.Member) bool {
	if config.RequiredRoleID == "" {
		return true
	}
	for _, roleID := range member.Roles {
		if roleID == config.RequiredRoleID {
			return true
		}
	}
	return false
}

func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Autocomplete is gated on the channel too: it queries Authentik for group
	// and user names, so serving it outside the designated channel would leak
	// that data to any channel the bot can see.
	if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
		if !isCommandAllowed(i.ChannelID) {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionApplicationCommandAutocompleteResult,
				Data: &discordgo.InteractionResponseData{
					Choices: []*discordgo.ApplicationCommandOptionChoice{},
				},
			})
			return
		}
		handleAutocomplete(s, i)
		return
	}

	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}

	if !isCommandAllowed(i.ChannelID) {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ Commands can only be used in the designated channel.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	if i.Member != nil && !isUserAuthorized(i.Member) {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ You do not have permission to use this command.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	switch i.ApplicationCommandData().Name {
	case "invite":
		handleInviteCommand(s, i)
	case "group":
		handleGroupCommand(s, i)
	case "user":
		handleUserCommand(s, i)
	}
}

func handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()

	if data.Name != "group" {
		return
	}

	// Return empty choices to unauthorized members to prevent user/group enumeration
	if i.Member != nil && !isUserAuthorized(i.Member) {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{
				Choices: []*discordgo.ApplicationCommandOptionChoice{},
			},
		})
		return
	}

	var focusedOption string
	var currentInput string

	for _, opt := range data.Options {
		if opt.Focused {
			focusedOption = opt.Name
			currentInput = strings.ToLower(opt.StringValue())
			break
		}
	}

	var choices []*discordgo.ApplicationCommandOptionChoice

	switch focusedOption {
	case "group":
		groups, err := getAllGroups()
		if err != nil {
			log.Printf("Autocomplete error fetching groups: %v", err)
			choices = []*discordgo.ApplicationCommandOptionChoice{}
		} else {
			for _, g := range groups {
				if currentInput == "" || strings.Contains(strings.ToLower(g.Name), currentInput) {
					choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
						Name:  g.Name,
						Value: g.Name,
					})
					if len(choices) >= 25 {
						break
					}
				}
			}
		}

	case "username":
		users, err := getAllUsers()
		if err != nil {
			log.Printf("Autocomplete error fetching users: %v", err)
			choices = []*discordgo.ApplicationCommandOptionChoice{}
		} else {
			immutable := map[string]bool{}
			for _, u := range config.ImmutableUsernames {
				immutable[strings.ToLower(strings.TrimSpace(u))] = true
			}

			for _, u := range users {
				if immutable[strings.ToLower(u.Username)] {
					continue
				}
				if currentInput == "" || strings.Contains(strings.ToLower(u.Username), currentInput) {
					choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
						Name:  u.Username,
						Value: u.Username,
					})
					if len(choices) >= 25 {
						break
					}
				}
			}
		}
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{
			Choices: choices,
		},
	}); err != nil {
		log.Printf("Error responding to autocomplete: %v", err)
	}
}

func handleInviteCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Ephemeral: only the invoker sees the invite link
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})

	expiration := config.DefaultExpiration
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "expiration" {
			expiration = opt.IntValue()
			break
		}
	}
	if expiration < 1 {
		expiration = 1
	}
	if expiration > 8760 {
		expiration = 8760
	}

	inviteLink, err := generateAuthentikInvite(expiration)
	if err != nil {
		log.Printf("Error generating invite: %v", err)
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ Failed to generate invite: " + err.Error()),
		})
		return
	}

	message := fmt.Sprintf("✅ Invite generated (expires in %d hours):\n```\n%s\n```", expiration, inviteLink)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: stringPtr(message),
	})
}

func generateAuthentikInvite(expirationHours int64) (string, error) {
	expiresAt := time.Now().Add(time.Duration(expirationHours) * time.Hour)
	expiresStr := expiresAt.Format("2006-01-02T15:04:05Z")

	inviteName := "invite-" + strings.ReplaceAll(uuid.New().String(), "-", "")[:12]

	inviteReq := InviteRequest{
		Name:      inviteName,
		Expires:   expiresStr,
		SingleUse: true,
		FixedData: map[string]interface{}{},
	}

	body, err := json.Marshal(inviteReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v3/stages/invitation/invitations/", strings.TrimRight(config.AuthentikURL, "/"))
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := apiClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request to Authentik: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		log.Printf("Authentik API returned status %d: %s", resp.StatusCode, string(respBody))
		return "", fmt.Errorf("authentik API returned status %d", resp.StatusCode)
	}

	var inviteResp InviteResponse
	if err := json.Unmarshal(respBody, &inviteResp); err != nil {
		log.Printf("Failed to parse response JSON: %s", string(respBody))
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if inviteResp.Pk == "" {
		log.Printf("Error: API response missing 'pk' field")
		return "", fmt.Errorf("authentik response missing invitation pk")
	}

	baseURL := strings.TrimRight(config.AuthentikURL, "/")
	inviteLink := fmt.Sprintf("%s/if/flow/%s/?itoken=%s", baseURL, config.AuthentikFlowSlug, inviteResp.Pk)

	log.Printf("Created invitation UUID: %s (expires: %s)", inviteResp.Pk, inviteResp.Expires)
	return inviteLink, nil
}

func stringPtr(s string) *string {
	return &s
}

func deleteMessageAfterDelay(s *discordgo.Session, channelID, messageID string, delay time.Duration) {
	time.Sleep(delay)
	if err := s.ChannelMessageDelete(channelID, messageID); err != nil {
		log.Printf("Error deleting message: %v", err)
		return
	}
	log.Printf("Message deleted after delay (ID: %s)", messageID)
}

func validateGroupActionInputs(action, groupName, username string) error {
	if action != "add" && action != "remove" {
		return nil
	}

	trimmedGroup := strings.TrimSpace(groupName)
	trimmedUser := strings.TrimSpace(username)

	if trimmedGroup == "" && trimmedUser == "" {
		return fmt.Errorf("group and username are required for %s action", action)
	}
	if trimmedGroup == "" {
		return fmt.Errorf("group is required for %s action", action)
	}
	if trimmedUser == "" {
		return fmt.Errorf("username is required for %s action", action)
	}

	return nil
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

	if action == "list" {
		groups, err := getAllGroups()
		if err != nil {
			log.Printf("Error fetching groups: %v", err)
			m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content: stringPtr("❌ Failed to fetch groups: " + err.Error()),
			})
			if m != nil {
				go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
			}
			return
		}

		sort.Slice(groups, func(a, b int) bool {
			return strings.ToLower(groups[a].Name) < strings.ToLower(groups[b].Name)
		})

		if len(groups) == 0 {
			m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content: stringPtr("No groups to display."),
			})
			if m != nil {
				go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
			}
			return
		}

		pages := buildGroupTablePages(groups)
		firstMsg, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: stringPtr(pages[0])})
		if firstMsg != nil {
			go deleteMessageAfterDelay(s, i.ChannelID, firstMsg.ID, 5*time.Minute)
		}
		for p := 1; p < len(pages); p++ {
			fm, _ := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: pages[p]})
			if fm != nil {
				go deleteMessageAfterDelay(s, fm.ChannelID, fm.ID, 5*time.Minute)
			}
		}
		return
	}

	if err := validateGroupActionInputs(action, groupName, username); err != nil {
		m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: stringPtr("❌ " + err.Error()),
		})
		if m != nil {
			go deleteMessageAfterDelay(s, i.ChannelID, m.ID, 5*time.Minute)
		}
		return
	}

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

	switch action {
	case "add":
		result, err = addUserToGroup(username, groupName)
	case "remove":
		result, err = removeUserFromGroup(username, groupName)
	default:
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

	m, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content: stringPtr("✅ " + result),
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

	resp, err := apiClient.Do(req)
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
	if err := json.Unmarshal(respBody, &usersResp); err != nil {
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

	resp, err := apiClient.Do(req)
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
	if err := json.Unmarshal(respBody, &groupsResp); err != nil {
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

	body, err := json.Marshal(map[string]int64{"pk": user.Pk})
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v3/core/groups/%s/add_user/", strings.TrimRight(config.AuthentikURL, "/"), group.Pk)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := apiClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
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

	body, err := json.Marshal(map[string]int64{"pk": user.Pk})
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/v3/core/groups/%s/remove_user/", strings.TrimRight(config.AuthentikURL, "/"), group.Pk)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := apiClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("authentik API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return fmt.Sprintf("User '%s' removed from group '%s'", username, groupName), nil
}

func getAllGroups() ([]Group, error) {
	all := []Group{}
	base := strings.TrimRight(config.AuthentikURL, "/")
	url := fmt.Sprintf("%s/api/v3/core/groups/?page_size=200", base)

	for {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := apiClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to send request: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("authentik API returned status %d: %s", resp.StatusCode, string(body))
		}

		var gr GroupsResponse
		if err := json.Unmarshal(body, &gr); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}
		all = append(all, gr.Results...)

		if strings.TrimSpace(gr.Next) == "" {
			break
		}
		url = gr.Next
	}

	return all, nil
}

// Build paginated table pages for groups within Discord 2000-char limits
func buildGroupTablePages(groups []Group) []string {
	maxName := 0
	for _, g := range groups {
		if l := len(g.Name); l > maxName {
			maxName = l
		}
	}
	if maxName > 44 {
		maxName = 44
	}
	if maxName < 8 {
		maxName = 8
	}

	header := fmt.Sprintf("| %-3s | %-*s | %s |\n", "#", maxName, "group name", "members")
	sep := "|-----+" + strings.Repeat("-", maxName+2) + "+---------|\n"

	pages := []string{}
	pageIdx := 1
	total := len(groups)
	i := 0
	for i < total {
		pageHeader := fmt.Sprintf("Groups (%d total) — Page %d", total, pageIdx)
		body := header + sep
		count := 0
		for i < total {
			rn := i + 1
			g := ellipsize(groups[i].Name, maxName)
			memberCount := len(groups[i].Members)
			line := fmt.Sprintf("| %3d | %-*s | %7d |\n", rn, maxName, g, memberCount)
			if len(pageHeader)+len("```\n")+len(body)+len(line)+len("```") > 1900 {
				break
			}
			body += line
			i++
			count++
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
	firstMsg, _ := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: stringPtr(pages[0])})
	if firstMsg != nil {
		go deleteMessageAfterDelay(s, i.ChannelID, firstMsg.ID, 5*time.Minute)
	}
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

	for {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+config.AuthentikToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := apiClient.Do(req)
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
	type row struct{ Username, FullName string }
	rows := make([]row, 0, len(users))
	for _, u := range users {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			name = u.Email
		}
		rows = append(rows, row{Username: u.Username, FullName: name})
	}

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

	header := fmt.Sprintf("| %-3s | %-*s | %-*s |\n", "#", maxUser, "username", maxName, "full name")
	sep := "|-----+" + strings.Repeat("-", maxUser+2) + "+" + strings.Repeat("-", maxName+2) + "|\n"

	pages := []string{}
	pageIdx := 1
	total := len(rows)
	i := 0
	for i < total {
		pageHeader := fmt.Sprintf("Users (%d, excluding immutable) — Page %d", total, pageIdx)
		body := header + sep
		count := 0
		for i < total {
			rn := i + 1
			u := ellipsize(rows[i].Username, maxUser)
			n := ellipsize(rows[i].FullName, maxName)
			line := fmt.Sprintf("| %3d | %-*s | %-*s |\n", rn, maxUser, u, maxName, n)
			if len(pageHeader)+len("```\n")+len(body)+len(line)+len("```") > 1900 {
				break
			}
			body += line
			i++
			count++
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
