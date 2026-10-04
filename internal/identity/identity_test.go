package identity

import (
	"strings"
	"testing"
)

func TestDecide(t *testing.T) {
	const svcUID = 995
	tests := []struct {
		name         string
		cmd          string
		euid         int
		serviceKnown bool
		dryRun       bool
		want         Action
		msg          string
	}{
		// Day-to-day commands.
		{"root runs deploy -> drop", "deploy", 0, true, false, Drop, ""},
		{"root runs rollback -> drop", "rollback", 0, true, false, Drop, ""},
		{"root runs status -> drop", "status", 0, true, false, Drop, ""},
		{"root runs check -> drop", "check", 0, true, false, Drop, ""},
		{"root runs list -> drop", "list", 0, true, false, Drop, ""},
		{"shipit runs deploy", "deploy", svcUID, true, false, Proceed, ""},
		{"shipit runs serve (systemd)", "serve", svcUID, true, false, Proceed, ""},
		{"other user runs deploy", "deploy", 1000, true, false, Refuse, "sudo shipit deploy"},
		{"other user runs status", "status", 1000, true, false, Refuse, "sudo shipit status"},

		// Installation management.
		{"root runs self-update", "self-update", 0, true, false, Proceed, ""},
		{"root runs uninstall", "uninstall", 0, true, false, Proceed, ""},
		{"shipit runs self-update", "self-update", svcUID, true, false, Refuse, "must be run as root"},
		{"other runs uninstall", "uninstall", 1000, true, false, Refuse, "sudo shipit uninstall"},
		{"dry-run self-update unprivileged", "self-update", 1000, true, true, Proceed, ""},
		{"dry-run does not relax deploy", "deploy", 1000, true, true, Refuse, "must run as the shipit user"},

		// No shipit user on this machine.
		{"dev machine, normal user", "deploy", 501, false, false, Proceed, ""},
		{"not installed, root", "deploy", 0, false, false, Refuse, "does not exist"},
		{"not installed, root self-update", "self-update", 0, false, false, Proceed, ""},

		// Commands that need nothing.
		{"version as anyone", "version", 1000, true, false, Proceed, ""},
		{"help as anyone", "help", 1000, true, false, Proceed, ""},
		{"unknown command", "frobnicate", 1000, true, false, Proceed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, msg := Decide(tt.cmd, tt.euid, svcUID, tt.serviceKnown, tt.dryRun)
			if got != tt.want {
				t.Fatalf("action = %v, want %v (msg %q)", got, tt.want, msg)
			}
			if !strings.Contains(msg, tt.msg) {
				t.Errorf("msg = %q, want it to contain %q", msg, tt.msg)
			}
			if got == Refuse && msg == "" {
				t.Error("refusal without a message")
			}
		})
	}
}

func TestEveryCommandIsClassified(t *testing.T) {
	// A new subcommand that touches project files must not silently run as
	// root: it has to be listed here or in Commands on purpose.
	want := map[string]Need{
		"deploy": Service, "rollback": Service, "list": Service, "status": Service,
		"check": Service, "serve": Service, "self-update": Root, "uninstall": Root,
	}
	for cmd, need := range want {
		if Commands[cmd] != need {
			t.Errorf("%s: need = %v, want %v", cmd, Commands[cmd], need)
		}
	}
	if len(Commands) != len(want) {
		t.Errorf("Commands has %d entries, expected %d", len(Commands), len(want))
	}
}

func TestLookupMissingUser(t *testing.T) {
	// The shipit user does not exist on development machines or CI runners;
	// that must read as "not installed", not as an error.
	if _, _, _, ok, err := Lookup(); err != nil {
		t.Fatalf("Lookup error: %v", err)
	} else if ok {
		t.Skip("a user named shipit exists on this machine")
	}
}
