package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

func recoveryCommands() []*discordgo.ApplicationCommand {
	if recovery == nil {
		return nil
	}
	var commands []*discordgo.ApplicationCommand
	for _, entry := range []struct {
		name, description string
		enabled           bool
	}{
		{emailAction, "Preview native recovery email for one eligible exact username", recovery.cfg.EmailEnabled},
		{mfaAction, "Preview removal of Authentik TOTP enrollments for one eligible user", recovery.cfg.MFAEnabled},
	} {
		if !entry.enabled {
			continue
		}
		permissions := int64(0)
		contexts := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
		commands = append(commands, &discordgo.ApplicationCommand{Name: entry.name, Description: entry.description, DefaultMemberPermissions: &permissions, Contexts: &contexts, Options: []*discordgo.ApplicationCommandOption{{Type: discordgo.ApplicationCommandOptionString, Name: "username", Description: "Exact Authentik username; ordinary human accounts only", Required: true}}})
	}
	return commands
}
func isRecoveryInteraction(i *discordgo.InteractionCreate) bool {
	if i.Type == discordgo.InteractionApplicationCommand {
		name := i.ApplicationCommandData().Name
		return name == emailAction || name == mfaAction
	}
	if i.Type == discordgo.InteractionMessageComponent {
		return strings.HasPrefix(i.MessageComponentData().CustomID, "recovery:")
	}
	return false
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
		data := i.ApplicationCommandData()
		name := ""
		for _, opt := range data.Options {
			if opt.Name == "username" && opt.Type == discordgo.ApplicationCommandOptionString {
				name = opt.StringValue()
			}
		}
		id, p, err := recovery.preview(c, data.Name, name)
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
