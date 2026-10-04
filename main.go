package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/beyenpay/shipit/internal/check"
	"github.com/beyenpay/shipit/internal/config"
	"github.com/beyenpay/shipit/internal/deploy"
	"github.com/beyenpay/shipit/internal/server"
	"github.com/beyenpay/shipit/internal/uninstall"
)

// version is overridden at release time: -ldflags "-X main.version=v1.0.0".
var version = "dev"

const defaultConfigPath = "/etc/shipit/shipit.yaml"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("shipit", flag.ContinueOnError)
	fs.Usage = usage
	cfgPath := fs.String("c", envOr("SHIPIT_CONFIG", defaultConfigPath), "config file")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	rest := fs.Args()
	if len(rest) == 0 {
		usage()
		return 2
	}

	switch cmd := rest[0]; cmd {
	case "version":
		fmt.Println("shipit", version)
		return 0
	case "check":
		return cmdCheck(*cfgPath, rest[1:])
	case "deploy", "rollback":
		return cmdSwitch(*cfgPath, cmd, rest[1:])
	case "list":
		return cmdList(*cfgPath, rest[1:])
	case "status":
		return cmdStatus(*cfgPath)
	case "serve":
		return cmdServe(*cfgPath)
	case "uninstall":
		return cmdUninstall(*cfgPath, rest[1:])
	case "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "shipit: unknown command %q\n\n", cmd)
		usage()
		return 2
	}
}

func cmdCheck(path string, args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	offline := fs.Bool("offline", false, "skip checks that need the network")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(path)
	if err != nil {
		printErr(err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	failures := check.Print(os.Stdout, check.New(cfg, path, *offline).All(ctx))
	if failures > 0 {
		fmt.Printf("\n%d problem(s) found\n", failures)
		return 1
	}
	fmt.Println("\nAll good")
	return 0
}

func cmdUninstall(cfgPath string, args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	var o uninstall.Options
	fs.BoolVar(&o.Purge, "purge", false, "also remove /etc/shipit (config, secret, tokens)")
	fs.BoolVar(&o.DeleteProjects, "delete-projects", false, "with --purge: also delete project directories")
	fs.BoolVar(&o.Yes, "y", false, "do not ask for confirmation")
	fs.BoolVar(&o.DryRun, "dry-run", false, "show what would be done, change nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if cfg, err := config.Load(cfgPath); err == nil {
		for _, name := range cfg.Names() {
			o.Projects = append(o.Projects, cfg.Projects[name])
		}
	} else if o.DeleteProjects {
		printErr(fmt.Errorf("--delete-projects needs a readable config to know which directories to delete: %w", err))
		return 1
	}

	o.Paths = uninstall.DefaultPaths
	o.In, o.Out = os.Stdin, os.Stdout
	o.IsRoot = os.Geteuid() == 0
	o.Run = func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := uninstall.Run(o); err != nil {
		printErr(err)
		return 1
	}
	return 0
}

// loadDeployer loads the config the same way the webhook server will:
// fresh on every call, failing on any error.
func loadDeployer(path string) (*deploy.Deployer, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return deploy.New(cfg, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[shipit] "+format+"\n", a...)
	}), nil
}

func cmdServe(cfgPath string) int {
	cfg, err := config.Load(cfgPath)
	if err == nil {
		err = cfg.ValidateServe()
	}
	if err == nil {
		err = config.CheckFileMode(cfgPath)
	}
	if err != nil {
		printErr(err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("shipit ")
	log.Printf("%s starting, config %s", version, cfgPath)
	if err := server.New(cfgPath).Run(ctx, cfg.Listen); err != nil {
		printErr(err)
		return 1
	}
	return 0
}

func cmdSwitch(cfgPath, cmd string, args []string) int {
	if len(args) < 1 || len(args) > 2 {
		fmt.Fprintf(os.Stderr, "usage: shipit %s <project> [tag]\n", cmd)
		return 2
	}
	d, err := loadDeployer(cfgPath)
	if err != nil {
		printErr(err)
		return 1
	}
	tag := ""
	if len(args) == 2 {
		tag = args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, deploy.DefaultTimeout)
	defer cancel()

	run := d.Deploy
	if cmd == "rollback" {
		run = d.Rollback
	}
	got, err := run(ctx, args[0], tag)
	if err != nil {
		printErr(err)
		return 1
	}
	fmt.Printf("✔ %s: %s is live (%s)\n", args[0], got, cmd)
	return 0
}

func cmdList(cfgPath string, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: shipit list <project>")
		return 2
	}
	d, err := loadDeployer(cfgPath)
	if err != nil {
		printErr(err)
		return 1
	}
	st, err := d.Status(args[0])
	if err != nil {
		printErr(err)
		return 1
	}
	tags, err := d.Releases(args[0])
	if err != nil {
		printErr(err)
		return 1
	}
	for _, tag := range tags {
		mark := ""
		switch tag {
		case st.Current:
			mark = "(current)"
		case st.Previous:
			mark = "(previous)"
		}
		fmt.Printf("  %-24s %s\n", tag, mark)
	}
	return 0
}

func cmdStatus(cfgPath string) int {
	d, err := loadDeployer(cfgPath)
	if err != nil {
		printErr(err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tCURRENT\tPREVIOUS\tSERVICE")
	code := 0
	for _, name := range d.Cfg.Names() {
		p := d.Cfg.Projects[name]
		st, err := d.Status(name)
		if err != nil {
			fmt.Fprintf(tw, "%s\t!\t!\t%v\n", name, err)
			code = 1
			continue
		}
		svc := "-"
		if p.Service != "" {
			svc = deploy.ServiceState(ctx, p.Service)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, dash(st.Current), dash(st.Previous), svc)
	}
	tw.Flush()
	return code
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printErr(err error) {
	for _, line := range strings.Split(err.Error(), "\n") {
		fmt.Fprintf(os.Stderr, "✘ %s\n", line)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprint(os.Stderr, `shipit - pull-based deploys from GitHub Releases

Usage:
  shipit [-c config] <command> [args]

Commands:
  deploy <project> [tag]     deploy a release (default: latest)
  rollback <project> [tag]   switch back to the previous (or given) version
  list <project>             list local releases
  status                     show the current version of every project
  serve                      run the webhook server
  check [-offline]           validate config, systemd units, sudoers, GitHub access
  uninstall [flags]          remove shipit (-purge, -delete-projects, -y, -dry-run)
  version                    print version

-c must come before the command. The config path is taken from -c, then
$SHIPIT_CONFIG, then `+defaultConfigPath+`.
`)
}
