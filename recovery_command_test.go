package main

import (
	"github.com/bwmarrin/discordgo"
	"testing"
)

func groupedRecoveryData(group, user string) discordgo.ApplicationCommandInteractionData {
	return discordgo.ApplicationCommandInteractionData{Name: authctlCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: group, Type: discordgo.ApplicationCommandOptionSubCommandGroup, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: "reset", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: "user", Type: discordgo.ApplicationCommandOptionString, Value: user}}}}}}}
}
func TestRecoveryGroupedCommandTree(t *testing.T) {
	old := recovery
	defer func() { recovery = old }()
	f := newRecoveryFixture(t)
	recovery = f.r
	commands := recoveryCommands()
	if len(commands) != 1 || commands[0].Name != authctlCommand {
		t.Fatal("expected one authctl root")
	}
	root := commands[0]
	if root.DefaultMemberPermissions == nil || *root.DefaultMemberPermissions != 0 || root.Contexts == nil || len(*root.Contexts) != 1 || (*root.Contexts)[0] != discordgo.InteractionContextGuild {
		t.Fatal("root security/context changed")
	}
	if len(root.Options) != 2 {
		t.Fatal("expected password and MFA groups")
	}
	for i, want := range []string{passwordCommand, mfaCommand} {
		g := root.Options[i]
		if g.Type != discordgo.ApplicationCommandOptionSubCommandGroup || g.Name != want || len(g.Options) != 1 {
			t.Fatal("wrong recovery group")
		}
		sub := g.Options[0]
		if sub.Name != "reset" || sub.Type != discordgo.ApplicationCommandOptionSubCommand || len(sub.Options) != 1 {
			t.Fatal("missing reset subcommand")
		}
		user := sub.Options[0]
		if user.Name != "user" || user.Type != discordgo.ApplicationCommandOptionString || !user.Required || user.Autocomplete {
			t.Fatal("wrong exact user argument")
		}
	}
	f.r.cfg.EmailEnabled = false
	c := recoveryCommands()
	if len(c) != 1 || len(c[0].Options) != 1 || c[0].Options[0].Name != mfaCommand {
		t.Fatal("feature switch changed")
	}
	f.r.cfg.MFAEnabled = false
	if len(recoveryCommands()) != 0 {
		t.Fatal("disabled root registered")
	}
}
func TestRecoveryGroupedRoutingAndAuthorization(t *testing.T) {
	for _, root := range []string{passwordCommand, mfaCommand} {
		for _, which := range []string{"allowed", "caller", "channel", "guild", "dm", "disabled"} {
			t.Run(root+"/"+which, func(t *testing.T) {
				f := newRecoveryFixture(t)
				c := f.ctx
				switch which {
				case "caller":
					c.Caller = "999"
				case "channel":
					c.Channel = "999"
				case "guild":
					c.Guild = "999"
				case "dm":
					c.Guild = ""
				case "disabled":
					f.r.cfg.EmailEnabled = false
					f.r.cfg.MFAEnabled = false
				}
				data := groupedRecoveryData(root, "alice")
				interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: discordgo.InteractionApplicationCommand, Data: data}}
				if !isRecoveryInteraction(interaction) {
					t.Fatal("grouped command bypassed recovery handler")
				}
				id, p, err := f.r.previewCommand(c, data)
				if which != "allowed" {
					if err == nil || len(f.requests) != 0 {
						t.Fatal("unauthorized grouped command reached HTTP")
					}
					return
				}
				want := emailAction
				if root == mfaCommand {
					want = mfaAction
				}
				if err != nil || p.Action != want || p.User.Username != "alice" || id == "" || len(f.writes) != 0 {
					t.Fatal("grouped command did not map to unchanged preview")
				}
				if _, err = f.r.confirm(c, id, false); err != nil {
					t.Fatal(err)
				}
				writes := 1
				if root == mfaCommand {
					writes = 2
				}
				if len(f.writes) != writes {
					t.Fatal("confirmed action semantics changed")
				}
			})
		}
	}
	for _, root := range []string{"invite", "group", "user"} {
		i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: discordgo.InteractionApplicationCommand, Data: discordgo.ApplicationCommandInteractionData{Name: root}}}
		if isRecoveryInteraction(i) {
			t.Fatal("legacy command intercepted")
		}
	}
}
func TestRecoveryGroupedMalformedAndRetiredCommandsFailClosed(t *testing.T) {
	for _, which := range []string{"retired email", "retired MFA", "retired password root", "retired mfa root", "missing group", "nil group", "unknown group", "group wrong type", "missing reset", "nil subcommand", "wrong action", "action choice", "missing user", "nil user", "username alias", "wrong user type", "wrong value type", "duplicate user", "extra subcommand", "extra group"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			d := groupedRecoveryData(passwordCommand, "alice")
			switch which {
			case "retired email":
				d.Name = emailAction
			case "retired MFA":
				d.Name = mfaAction
			case "retired password root":
				d.Name = passwordCommand
			case "retired mfa root":
				d.Name = mfaCommand
			case "missing group":
				d.Options = nil
			case "nil group":
				d.Options[0] = nil
			case "unknown group":
				d.Options[0].Name = "other"
			case "group wrong type":
				d.Options[0].Type = discordgo.ApplicationCommandOptionSubCommand
			case "missing reset":
				d.Options[0].Options = nil
			case "nil subcommand":
				d.Options[0].Options[0] = nil
			case "wrong action":
				d.Options[0].Options[0].Name = "remove"
			case "action choice":
				d.Options[0].Options[0].Type = discordgo.ApplicationCommandOptionString
				d.Options[0].Options[0].Value = "reset"
			case "missing user":
				d.Options[0].Options[0].Options = nil
			case "nil user":
				d.Options[0].Options[0].Options[0] = nil
			case "username alias":
				d.Options[0].Options[0].Options[0].Name = "username"
			case "wrong user type":
				d.Options[0].Options[0].Options[0].Type = discordgo.ApplicationCommandOptionUser
			case "wrong value type":
				d.Options[0].Options[0].Options[0].Value = true
			case "duplicate user":
				d.Options[0].Options[0].Options = append(d.Options[0].Options[0].Options, d.Options[0].Options[0].Options[0])
			case "extra subcommand":
				d.Options[0].Options = append(d.Options[0].Options, d.Options[0].Options[0])
			case "extra group":
				d.Options = append(d.Options, d.Options[0])
			}
			if _, _, err := f.r.previewCommand(f.ctx, d); err == nil || len(f.requests) != 0 || len(f.r.pending) != 0 {
				t.Fatal("malformed command reached eligibility/confirmation")
			}
			if got := f.audit.statuses(); len(got) != 1 || got[0] != "denied" {
				t.Fatal("invalid command not audited")
			}
		})
	}
}
