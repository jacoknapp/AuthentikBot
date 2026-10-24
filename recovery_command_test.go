package main

import (
	"github.com/bwmarrin/discordgo"
	"testing"
)

func recoveryCommandData(command, user string) discordgo.ApplicationCommandInteractionData {
	return discordgo.ApplicationCommandInteractionData{Name: command, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: "action", Type: discordgo.ApplicationCommandOptionString, Value: "reset"}, {Name: "user", Type: discordgo.ApplicationCommandOptionString, Value: user}}}
}
func TestRecoveryTopLevelCommandTree(t *testing.T) {
	old := recovery
	defer func() { recovery = old }()
	f := newRecoveryFixture(t)
	recovery = f.r
	commands := recoveryCommands()
	if len(commands) != 2 {
		t.Fatal("expected password and mfa roots only")
	}
	for i, want := range []string{passwordCommand, mfaCommand} {
		root := commands[i]
		if root.Name != want || root.Name == authctlCommand || root.Type != discordgo.ChatApplicationCommand || len(root.Options) != 2 {
			t.Fatal("wrong recovery root/tree")
		}
		if root.DefaultMemberPermissions == nil || *root.DefaultMemberPermissions != 0 || root.Contexts == nil || len(*root.Contexts) != 1 || (*root.Contexts)[0] != discordgo.InteractionContextGuild {
			t.Fatal("root authorization context changed")
		}
		action, user := root.Options[0], root.Options[1]
		if action.Name != "action" || action.Type != discordgo.ApplicationCommandOptionString || !action.Required || action.Autocomplete || len(action.Choices) != 1 || action.Choices[0].Name != "reset" || action.Choices[0].Value != "reset" {
			t.Fatal("wrong reset-only action choice")
		}
		if user.Name != "user" || user.Type != discordgo.ApplicationCommandOptionString || !user.Required || !user.Autocomplete {
			t.Fatal("wrong eligible-user autocomplete option")
		}
	}
	f.r.cfg.EmailEnabled = false
	c := recoveryCommands()
	if len(c) != 1 || c[0].Name != mfaCommand {
		t.Fatal("feature switch changed")
	}
	f.r.cfg.MFAEnabled = false
	if len(recoveryCommands()) != 0 {
		t.Fatal("disabled commands registered")
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
				data := recoveryCommandData(root, "alice")
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
func TestRecoveryTopLevelMalformedAndRetiredCommandsFailClosed(t *testing.T) {
	for _, which := range []string{"old email", "old MFA", "authctl wrapper", "missing action", "missing user", "nil action", "nil user", "wrong root", "nested reset", "wrong action name", "wrong action value", "wrong action type", "wrong action value type", "nested action", "username alias", "wrong user type", "wrong user value type", "nested user", "duplicate action", "duplicate user", "extra option"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			d := recoveryCommandData(passwordCommand, "alice")
			options := d.Options
			switch which {
			case "old email":
				d.Name = emailAction
			case "old MFA":
				d.Name = mfaAction
			case "authctl wrapper":
				d.Name = authctlCommand
				d.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: passwordCommand, Type: discordgo.ApplicationCommandOptionSubCommand, Options: options}}
			case "missing action":
				d.Options = options[1:]
			case "missing user":
				d.Options = options[:1]
			case "nil action":
				options[0] = nil
			case "nil user":
				options[1] = nil
			case "wrong root":
				d.Name = "other"
			case "nested reset":
				d.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: "reset", Type: discordgo.ApplicationCommandOptionSubCommand, Options: options}}
			case "wrong action name":
				options[0].Name = "operation"
			case "wrong action value":
				options[0].Value = "remove"
			case "wrong action type":
				options[0].Type = discordgo.ApplicationCommandOptionSubCommand
			case "wrong action value type":
				options[0].Value = true
			case "nested action":
				options[0].Options = []*discordgo.ApplicationCommandInteractionDataOption{options[1]}
			case "username alias":
				options[1].Name = "username"
			case "wrong user type":
				options[1].Type = discordgo.ApplicationCommandOptionUser
			case "wrong user value type":
				options[1].Value = true
			case "nested user":
				options[1].Options = []*discordgo.ApplicationCommandInteractionDataOption{options[0]}
			case "duplicate action":
				options[1] = options[0]
			case "duplicate user":
				options[0] = options[1]
			case "extra option":
				d.Options = append(options, options[0])
			}
			if _, _, err := f.r.previewCommand(f.ctx, d); err == nil || len(f.requests) != 0 || len(f.r.pending) != 0 {
				t.Fatal("invalid command reached eligibility/confirmation")
			}
			if got := f.audit.statuses(); len(got) != 1 || got[0] != "denied" {
				t.Fatal("invalid command not audited")
			}
		})
	}
}
func TestRecoveryActionOptionOrder(t *testing.T) {
	for _, name := range []string{passwordCommand, mfaCommand} {
		data := recoveryCommandData(name, "alice")
		data.Options[0], data.Options[1] = data.Options[1], data.Options[0]
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
