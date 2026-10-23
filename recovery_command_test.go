package main

import (
	"github.com/bwmarrin/discordgo"
	"testing"
)

func groupedRecoveryData(subcommand, user string) discordgo.ApplicationCommandInteractionData {
	return discordgo.ApplicationCommandInteractionData{Name: authctlCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: subcommand, Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: "action", Type: discordgo.ApplicationCommandOptionString, Value: "reset"}, {Name: "user", Type: discordgo.ApplicationCommandOptionString, Value: user}}}}}
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
		t.Fatal("expected password and MFA subcommands")
	}
	for i, want := range []string{passwordCommand, mfaCommand} {
		sub := root.Options[i]
		if sub.Type != discordgo.ApplicationCommandOptionSubCommand || sub.Name != want || len(sub.Options) != 2 {
			t.Fatal("wrong recovery subcommand")
		}
		action, user := sub.Options[0], sub.Options[1]
		if action.Name != "action" || action.Type != discordgo.ApplicationCommandOptionString || !action.Required || action.Autocomplete || len(action.Choices) != 1 || action.Choices[0].Name != "reset" || action.Choices[0].Value != "reset" {
			t.Fatal("wrong reset-only action choice")
		}
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
	for _, which := range []string{"retired email", "retired MFA", "retired password root", "retired mfa root", "missing subcommand", "nil subcommand", "unknown subcommand", "retired nested reset", "missing action", "missing user", "nil action", "nil user", "wrong action name", "wrong action value", "wrong action type", "wrong action value type", "nested action", "username alias", "wrong user type", "wrong user value type", "nested user", "duplicate action", "duplicate user", "extra option", "extra subcommand"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			d := groupedRecoveryData(passwordCommand, "alice")
			sub := d.Options[0]
			switch which {
			case "retired email":
				d.Name = emailAction
			case "retired MFA":
				d.Name = mfaAction
			case "retired password root":
				d.Name = passwordCommand
			case "retired mfa root":
				d.Name = mfaCommand
			case "missing subcommand":
				d.Options = nil
			case "nil subcommand":
				d.Options[0] = nil
			case "unknown subcommand":
				sub.Name = "other"
			case "retired nested reset":
				sub.Type = discordgo.ApplicationCommandOptionSubCommandGroup
				sub.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: "reset", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{sub.Options[1]}}}
			case "missing action":
				sub.Options = sub.Options[1:]
			case "missing user":
				sub.Options = sub.Options[:1]
			case "nil action":
				sub.Options[0] = nil
			case "nil user":
				sub.Options[1] = nil
			case "wrong action name":
				sub.Options[0].Name = "operation"
			case "wrong action value":
				sub.Options[0].Value = "remove"
			case "wrong action type":
				sub.Options[0].Type = discordgo.ApplicationCommandOptionSubCommand
			case "wrong action value type":
				sub.Options[0].Value = true
			case "nested action":
				sub.Options[0].Options = []*discordgo.ApplicationCommandInteractionDataOption{sub.Options[1]}
			case "username alias":
				sub.Options[1].Name = "username"
			case "wrong user type":
				sub.Options[1].Type = discordgo.ApplicationCommandOptionUser
			case "wrong user value type":
				sub.Options[1].Value = true
			case "nested user":
				sub.Options[1].Options = []*discordgo.ApplicationCommandInteractionDataOption{sub.Options[0]}
			case "duplicate action":
				sub.Options[1] = sub.Options[0]
			case "duplicate user":
				sub.Options[0] = sub.Options[1]
			case "extra option":
				sub.Options = append(sub.Options, sub.Options[0])
			case "extra subcommand":
				d.Options = append(d.Options, sub)
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
func TestRecoveryActionOptionOrder(t *testing.T) {
	for _, name := range []string{passwordCommand, mfaCommand} {
		data := groupedRecoveryData(name, "alice")
		sub := data.Options[0]
		sub.Options[0], sub.Options[1] = sub.Options[1], sub.Options[0]
		action, user, ok := parseRecoveryCommand(data)
		want := emailAction
		if name == mfaCommand {
			want = mfaAction
		}
		if !ok || action != want || user != "alice" {
			t.Fatal("option order changed meaning")
		}
	}
}
