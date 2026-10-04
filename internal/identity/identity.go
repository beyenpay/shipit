// Package identity makes sure each command runs as the right account.
//
// Day-to-day commands (deploy, rollback, status, ...) must run as the shipit
// user: they create files in the project directories, and a file owned by root
// there would later break deploys started by the webhook. When root runs one of
// them, shipit drops to the shipit user by itself and re-executes. Commands
// that manage the installation itself (self-update, uninstall) need root.
package identity

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// ServiceUser is the account shipit and the projects run as. It is a constant:
// nothing a caller passes in can change who root drops to.
const ServiceUser = "shipit"

// Need is the account a command must run as.
type Need int

const (
	// Any means the command works as whoever runs it.
	Any Need = iota
	// Service means the command must run as the shipit user.
	Service
	// Root means the command must run as root.
	Root
)

// Commands lists what each command needs. A command that is not listed needs
// nothing.
var Commands = map[string]Need{
	"deploy":      Service,
	"rollback":    Service,
	"list":        Service,
	"status":      Service,
	"check":       Service,
	"serve":       Service,
	"self-update": Root,
	"uninstall":   Root,
}

// Action is what to do about a mismatch.
type Action int

const (
	// Proceed means the current account is fine.
	Proceed Action = iota
	// Drop means: re-execute as the shipit user.
	Drop
	// Refuse means: stop with the returned message.
	Refuse
)

// Decide compares the account a command needs with the one it runs as.
// dryRun lets read-only previews of root-only commands run unprivileged.
func Decide(cmd string, euid int, serviceUID int, serviceKnown, dryRun bool) (Action, string) {
	switch Commands[cmd] {
	case Root:
		if euid == 0 || dryRun {
			return Proceed, ""
		}
		return Refuse, fmt.Sprintf("%s must be run as root: sudo shipit %s", cmd, cmd)

	case Service:
		switch {
		case !serviceKnown:
			// No shipit user on this machine (development, or not installed).
			// There is nothing to switch to; do not guess.
			if euid == 0 {
				return Refuse, fmt.Sprintf("refusing to run %s as root: files it creates would belong to root, and the user %q does not exist (run install.sh first)", cmd, ServiceUser)
			}
			return Proceed, ""
		case euid == serviceUID:
			return Proceed, ""
		case euid == 0:
			return Drop, ""
		default:
			return Refuse, fmt.Sprintf("%s must run as the %s user: sudo shipit %s", cmd, ServiceUser, cmd)
		}
	}
	return Proceed, ""
}

// Lookup returns the uid and gid of the shipit user.
func Lookup() (uid, gid int, groups []int, ok bool, err error) {
	u, err := user.Lookup(ServiceUser)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return 0, 0, nil, false, nil
		}
		return 0, 0, nil, false, err
	}
	if uid, err = strconv.Atoi(u.Uid); err != nil {
		return 0, 0, nil, false, err
	}
	if gid, err = strconv.Atoi(u.Gid); err != nil {
		return 0, 0, nil, false, err
	}
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.Atoi(g); err == nil {
				groups = append(groups, n)
			}
		}
	}
	if len(groups) == 0 {
		groups = []int{gid}
	}
	return uid, gid, groups, true, nil
}

// DropAndExec replaces the current process with the same command line running
// as the shipit user. It returns only if that failed.
func DropAndExec() error {
	uid, gid, groups, ok, err := Lookup()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q does not exist", ServiceUser)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Order matters: groups and gid first, uid last, since after setuid the
	// process may no longer be allowed to change them.
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("drop groups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("drop gid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("drop uid: %w", err)
	}
	if syscall.Getuid() != uid || syscall.Geteuid() != uid {
		return errors.New("failed to drop privileges")
	}
	// A shell-less home still gives the child a sane HOME/USER.
	env := append(os.Environ(), "USER="+ServiceUser, "LOGNAME="+ServiceUser)
	if u, err := user.Lookup(ServiceUser); err == nil && u.HomeDir != "" {
		env = append(env, "HOME="+u.HomeDir)
	}
	// The shipit user may not be allowed into root's current directory (for
	// example /root). Keep the directory when it is usable, so relative paths
	// given on the command line still work; otherwise start from /.
	if _, err := os.Stat("."); err != nil {
		if err := os.Chdir("/"); err != nil {
			return err
		}
	}
	return syscall.Exec(self, os.Args, env)
}
