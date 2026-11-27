package deploy

import (
	"errors"
	"os/user"
	"strings"
	"testing"
)

// fakeAccounts replaces the account lookups for one test.
func fakeAccounts(t *testing.T, byName map[string]string, byID map[string]string) {
	t.Helper()
	oldName, oldID := userByName, userByID
	t.Cleanup(func() { userByName, userByID = oldName, oldID })
	find := func(m map[string]string) func(string) (*user.User, error) {
		return func(key string) (*user.User, error) {
			uid, ok := m[key]
			if !ok {
				return nil, user.UnknownUserError(key)
			}
			return &user.User{Uid: uid, Username: key}, nil
		}
	}
	userByName, userByID = find(byName), find(byID)
}

func TestValidateRunner(t *testing.T) {
	fakeAccounts(t,
		map[string]string{"shipit": "990", "svc-api": "991", "sneaky": "0"},
		map[string]string{"991": "991", "0": "0", "5": "0"},
	)
	tests := []struct {
		name    string
		in      string
		wantErr string // empty: accepted
	}{
		{"dedicated account", "svc-api", ""},
		{"numeric uid", "991", ""},
		{"legacy shipit account is still allowed", "shipit", ""},
		{"no User= means root", "", "no User= set"},
		{"root by name", "root", "runs as root"},
		{"root by uid", "0", "runs as root"},
		{"another name for uid 0", "sneaky", "which is root"},
		{"another uid 0 spelled differently", "5", "which is root"},
		{"unknown account", "ghost", "cannot resolve User=ghost"},
		{"unknown numeric uid", "4242", "cannot resolve User=4242"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRunner(tt.in)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("ValidateRunner(%q) = %v, want nil", tt.in, err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("ValidateRunner(%q) accepted, want error containing %q", tt.in, tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("ValidateRunner(%q) = %v, want it to contain %q", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestValidateRunnerLookupFailure(t *testing.T) {
	// A lookup that fails for another reason than "no such user" must not
	// let the restart through.
	oldName := userByName
	t.Cleanup(func() { userByName = oldName })
	userByName = func(string) (*user.User, error) { return nil, errors.New("nss unavailable") }
	if err := ValidateRunner("svc-api"); err == nil || !strings.Contains(err.Error(), "nss unavailable") {
		t.Fatalf("got %v, want the lookup error", err)
	}
}

func TestRunnerLabel(t *testing.T) {
	tests := map[string]string{
		"svc-api": "svc-api",
		"shipit":  "shipit !",
		"root":    "root !",
		"0":       "0 !",
		"n/a":     "n/a",
	}
	for in, want := range tests {
		if got := RunnerLabel(in); got != want {
			t.Errorf("RunnerLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunnerRejectsBadNames(t *testing.T) {
	// These must be answered without ever invoking systemctl.
	for _, s := range []string{"", "-p"} {
		if got := Runner(t.Context(), s); got != "n/a" {
			t.Errorf("Runner(%q) = %q, want n/a", s, got)
		}
	}
}
