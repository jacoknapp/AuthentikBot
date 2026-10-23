package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const authctlCommand = "authctl"
const passwordCommand = "password"
const mfaCommand = "mfa"

func recoveryCommands() []*discordgo.ApplicationCommand {
	if recovery == nil {
		return nil
	}
	var subcommands []*discordgo.ApplicationCommandOption
	for _, entry := range []struct {
		name, description string
		enabled           bool
	}{
		{passwordCommand, "Password recovery operations", recovery.cfg.EmailEnabled},
		{mfaCommand, "TOTP recovery operations", recovery.cfg.MFAEnabled},
	} {
		if !entry.enabled {
			continue
		}
		subcommands = append(subcommands, &discordgo.ApplicationCommandOption{
			Type: discordgo.ApplicationCommandOptionSubCommand, Name: entry.name, Description: entry.description,
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "action", Description: "Recovery action", Required: true, Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: "reset", Value: "reset"}}},
				{Type: discordgo.ApplicationCommandOptionString, Name: "user", Description: "Exact Authentik username; ordinary human accounts only", Required: true},
			},
		})
	}
	if len(subcommands) == 0 {
		return nil
	}
	permissions := int64(0)
	contexts := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
	return []*discordgo.ApplicationCommand{{Name: authctlCommand, Description: "Authentik account recovery operations", DefaultMemberPermissions: &permissions, Contexts: &contexts, Options: subcommands}}
}
func isRecoveryInteraction(i *discordgo.InteractionCreate) bool {
	if i.Type == discordgo.InteractionApplicationCommand {
		name := i.ApplicationCommandData().Name
		// Retired flat names are routed here only to reject stale Discord clients.
		return name == authctlCommand || name == passwordCommand || name == mfaCommand || name == emailAction || name == mfaAction
	}
	if i.Type == discordgo.InteractionMessageComponent {
		return strings.HasPrefix(i.MessageComponentData().CustomID, "recovery:")
	}
	return false
}

// The Discord tree is presentation only: internal action IDs stay stable so
// authorization, confirmations, cooldowns and audit semantics do not change.
func parseRecoveryCommand(data discordgo.ApplicationCommandInteractionData) (action, user string, valid bool) {
	if data.Name != authctlCommand || len(data.Options) != 1 || data.Options[0] == nil {
		return "", "", false
	}
	sub := data.Options[0]
	if sub.Type != discordgo.ApplicationCommandOptionSubCommand || len(sub.Options) != 2 {
		return "", "", false
	}
	switch sub.Name {
	case passwordCommand:
		action = emailAction
	case mfaCommand:
		action = mfaAction
	default:
		return "", "", false
	}
	seen := map[string]bool{}
	for _, option := range sub.Options {
		if option == nil || option.Type != discordgo.ApplicationCommandOptionString || len(option.Options) != 0 || seen[option.Name] {
			return "", "", false
		}
		seen[option.Name] = true
		value, ok := option.Value.(string)
		if !ok {
			return "", "", false
		}
		switch option.Name {
		case "action":
			if value != "reset" {
				return "", "", false
			}
		case "user":
			user = value
		default:
			return "", "", false
		}
	}
	return action, user, seen["action"] && seen["user"]
}
func (r *recoveryService) previewCommand(c recoveryContext, data discordgo.ApplicationCommandInteractionData) (string, recoveryPending, error) {
	action, user, valid := parseRecoveryCommand(data)
	if !valid {
		err := errors.New("Use /authctl password action:reset user:<username> or /authctl mfa action:reset user:<username>")
		if e := r.record(c, "recovery-command", 0, "denied", 0); e != nil {
			err = e
		}
		return "", recoveryPending{}, err
	}
	return r.preview(c, action, user)
}

func recoveryCaller(i *discordgo.InteractionCreate) recoveryContext {
	c := recoveryContext{Channel: i.ChannelID, Guild: i.GuildID}
	if i.Member != nil && i.Member.User != nil && i.GuildID != "" {
		c.Caller = i.Member.User.ID
	}
	return c
}
func (r *recoveryService) previewMessage(p recoveryPending) string {
	target := fmt.Sprintf("%s (user ID %d)", p.User.Username, p.User.PK)
	if p.Action == emailAction {
		return fmt.Sprintf("Confirm password recovery EMAIL for %s? Authentik will initiate native recovery using the existing email address. Delivery is not guaranteed. Expires in two minutes.", target)
	}
	counts := map[string]int{}
	for _, d := range p.Devices {
		counts[d.Kind]++
	}
	var scope []string
	// Includes zero counts so the exact configured scope is visible.
	for _, k := range r.cfg.Factors {
		scope = append(scope, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return fmt.Sprintf("Confirm removal of %d Authentik TOTP enrollment(s) for %s? Exact scope: %s. Only the previewed device IDs will be deleted. Other factors and existing sessions remain. A retained passkey may allow login without TOTP setup. Expires in two minutes.", len(p.Devices), target, strings.Join(scope, ", "))
}
func handleRecoveryInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Always acknowledge ephemerally before HTTP work. Never log Discord errors:
	// they can contain interaction webhook tokens or raw response bodies.
	mentions := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: mentions}}); err != nil {
		log.Print("Recovery Discord acknowledgement failed")
		return
	}
	message := "Recovery disabled or unavailable."
	components := []discordgo.MessageComponent{}
	c := recoveryCaller(i)
	if recovery == nil {
		// No recovery file is configured when disabled. No Authentik requests.
		b, _ := json.Marshal(recoveryAuditEvent{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Action: "recovery", Caller: c.Caller, Status: "denied"})
		log.Print(string(b))
	} else if i.Type == discordgo.InteractionApplicationCommand {
		id, p, err := recovery.previewCommand(c, i.ApplicationCommandData())
		if err != nil {
			message = err.Error()
		} else {
			message = recovery.previewMessage(p)
			components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "Confirm", Style: discordgo.DangerButton, CustomID: "recovery:confirm:" + id},
				discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: "recovery:cancel:" + id},
			}}}
		}
	} else {
		parts := strings.Split(i.MessageComponentData().CustomID, ":")
		if len(parts) == 3 && (parts[1] == "confirm" || parts[1] == "cancel") && len(parts[2]) == 32 {
			result, err := recovery.confirm(c, parts[2], parts[1] == "cancel")
			message = result
			if err != nil {
				if message != "" {
					message += "\n"
				}
				message += err.Error()
			}
		} else {
			message = "Invalid recovery confirmation."
			_ = recovery.record(c, "recovery", 0, "denied", 0)
		}
	}
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &message, Components: &components, AllowedMentions: mentions}); err != nil {
		log.Print("Recovery Discord response failed")
	}
}
