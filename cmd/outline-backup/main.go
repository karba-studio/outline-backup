// Command outline-backup backs up and restores Outline wikis whose
// attachments live in an S3-compatible object store.
//
// Run `outline-backup init` first; it asks for everything else.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/karba-studio/outline-backup/internal/cfg"
	"github.com/karba-studio/outline-backup/internal/resticx"
	"github.com/karba-studio/outline-backup/internal/run"
	"github.com/karba-studio/outline-backup/internal/version"
)

const usage = `outline-backup - back up and restore Outline (database + S3 attachments)

USAGE
  outline-backup <command> [flags]

COMMANDS
  init        Ask questions and write a configuration (start here)
  check       Verify every part of the setup; writes nothing
  backup      Run a backup now
  snapshots   List stored snapshots
  restore     Put a snapshot back, onto this host or a fresh one
  agent       Stay running and back up on the configured schedule
  config      Print the resolved configuration
  version     Print the build version

COMMON FLAGS
  --config <path>    Configuration file
                     (default: $OMB_CONFIG, else ./omb/config.json,
                      else the per-user config directory)

Every command reads secrets.env from the configuration's directory.
`

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return errors.New("no command given")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "init":
		return cmdInit(ctx, args)
	case "check":
		return cmdCheck(ctx, args)
	case "backup":
		return cmdBackup(ctx, args)
	case "snapshots":
		return cmdSnapshots(ctx, args)
	case "restore":
		return cmdRestore(ctx, args)
	case "agent":
		return cmdAgent(ctx, args)
	case "config":
		return cmdConfig(ctx, args)
	case "version", "--version", "-v":
		fmt.Println(version.String())
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// defaultConfigPath resolves where the configuration lives.
func defaultConfigPath() string {
	if p := os.Getenv("OMB_CONFIG"); p != "" {
		return p
	}
	local := filepath.Join("omb", cfg.DefaultConfigName)
	if _, err := os.Stat(local); err == nil {
		return local
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return local
	}
	return filepath.Join(dir, "outline-backup", cfg.DefaultConfigName)
}

func newFlagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath(), "path to config.json")
	return fs, path
}

// load reads the config and its secrets sidecar.
func load(path string) (*cfg.Config, error) {
	c, err := cfg.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no configuration at %s\n"+
				"run `outline-backup init` to create one, or pass --config", path)
		}
		return nil, err
	}
	secrets := c.SecretsPath()
	if _, statErr := os.Stat(secrets); statErr == nil {
		if err := cfg.LoadSecrets(secrets); err != nil {
			return nil, err
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func cmdInit(ctx context.Context, args []string) error {
	fs, path := newFlagSet("init")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o755); err != nil {
		return err
	}
	return run.Init(ctx, *path, os.Stdout)
}

func cmdCheck(ctx context.Context, args []string) error {
	fs, path := newFlagSet("check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*path)
	if err != nil {
		return err
	}
	_, err = run.Check(ctx, c, os.Stdout)
	return err
}

func cmdBackup(ctx context.Context, args []string) error {
	fs, path := newFlagSet("backup")
	instance := fs.String("instance", "", "back up only this instance (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*path)
	if err != nil {
		return err
	}

	targets, err := selectInstances(c, *instance)
	if err != nil {
		return err
	}

	var failed int
	for _, in := range targets {
		fmt.Printf("\n\033[1m%s\033[0m\n", in.Name)
		res, err := run.Backup(ctx, c, in, os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  \033[31mfailed\033[0m: %v\n", err)
			failed++
			continue
		}
		fmt.Printf("  \033[32mdone\033[0m %d attachments, %s of dumps, %s\n",
			res.AssetFiles, byteCount(res.DumpBytes), res.Duration.Round(time.Second))
		for _, cp := range res.Copies {
			if cp.Err != nil {
				fmt.Printf("    \033[31m%-14s\033[0m %v\n", cp.Destination, cp.Err)
				continue
			}
			fmt.Printf("    \033[32m%-14s\033[0m snapshot %s\n", cp.Destination, cp.SnapshotID)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d instances failed", failed, len(targets))
	}
	return nil
}

func cmdSnapshots(ctx context.Context, args []string) error {
	fs, path := newFlagSet("snapshots")
	instance := fs.String("instance", "", "only this instance (default: all)")
	destination := fs.String("destination", "", "only this destination (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*path)
	if err != nil {
		return err
	}
	targets, err := selectInstances(c, *instance)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE\tDESTINATION\tSNAPSHOT\tWHEN")
	for _, in := range targets {
		targets, err := c.Targets(in)
		if err != nil {
			return err
		}
		for _, t := range targets {
			if *destination != "" && t.Name != *destination {
				continue
			}
			runner, err := resticx.NewRunner(t, os.Stderr)
			if err != nil {
				return err
			}
			snaps, err := runner.Snapshots(ctx, in.Name)
			if err != nil {
				return err
			}
			if len(snaps) == 0 {
				fmt.Fprintf(w, "%s\t%s\t-\t(no snapshots yet)\n", in.Name, t.Name)
				continue
			}
			for _, s := range snaps {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", in.Name, t.Name, s.ShortID,
					s.Time.Local().Format("2006-01-02 15:04"))
			}
		}
	}
	return w.Flush()
}

func cmdRestore(ctx context.Context, args []string) error {
	fs, path := newFlagSet("restore")
	instance := fs.String("instance", "", "which instance to restore (required)")
	destination := fs.String("destination", "", "which destination to restore from (required when there are several)")
	snapshot := fs.String("snapshot", "latest", "snapshot id, or latest")
	target := fs.String("target", "", "directory to restore into (default: a temporary one)")
	fetchOnly := fs.Bool("fetch-only", false, "download and decrypt only; write nothing back")
	skipDB := fs.Bool("skip-database", false, "do not touch Postgres")
	skipAssets := fs.Bool("skip-assets", false, "do not touch the object store")
	owner := fs.String("db-owner", "", "create this role as the database owner if missing")
	ownerPw := fs.String("db-owner-password", "", "password for --db-owner")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *instance == "" {
		return errors.New("--instance is required; run `snapshots` to see what exists")
	}
	c, err := load(*path)
	if err != nil {
		return err
	}
	in, err := c.Instance(*instance)
	if err != nil {
		return err
	}

	opt := run.RestoreOptions{
		Destination:     *destination,
		Snapshot:        *snapshot,
		Target:          *target,
		FetchOnly:       *fetchOnly,
		Database:        !*skipDB,
		Assets:          !*skipAssets,
		DBOwner:         *owner,
		DBOwnerPassword: *ownerPw,
	}

	if !opt.FetchOnly && !*yes {
		fmt.Printf("\nThis will overwrite live data for %q:\n", in.Name)
		if opt.Database {
			fmt.Printf("  - database %s on service %s\n", in.Database, c.Docker.PostgresService)
		}
		if opt.Assets {
			fmt.Printf("  - bucket %s at %s\n", in.Bucket, c.StorageFor(in).Endpoint)
		}
		fmt.Printf("\nType the instance name to continue: ")
		var typed string
		_, _ = fmt.Scanln(&typed)
		if typed != in.Name {
			return errors.New("cancelled")
		}
	}

	res, err := run.Restore(ctx, c, in, opt, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("\n\033[32mrestored\033[0m from %s\n", res.Target)
	for _, r := range res.Restored {
		fmt.Printf("  %s\n", r)
	}
	if opt.FetchOnly {
		fmt.Printf("\nNothing was written back. Inspect the files, then re-run without --fetch-only.\n")
	}
	return nil
}

func cmdAgent(ctx context.Context, args []string) error {
	fs, path := newFlagSet("agent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("%s on %s/%s\n", version.String(), runtime.GOOS, runtime.GOARCH)
	return run.Agent(ctx, c, os.Stdout)
}

func cmdConfig(ctx context.Context, args []string) error {
	fs, path := newFlagSet("config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("config   : %s\n", c.Path())
	fmt.Printf("secrets  : %s\n", c.SecretsPath())
	fmt.Printf("staging  : %s\n", c.Staging())
	fmt.Printf("stack    : compose project %q, postgres service %q\n",
		c.Docker.ComposeProject, c.Docker.PostgresService)
	fmt.Printf("storage  : %s\n", c.Storage.Endpoint)
	fmt.Printf("schedule : %s\n", c.Schedule.Describe())
	if next := c.Schedule.NextRun(time.Now()); !next.IsZero() {
		fmt.Printf("next run : %s\n", next.Format("Mon 2006-01-02 15:04 MST"))
	}
	fmt.Println("instances:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tDATABASE\tBUCKET\tBACKS UP TO")
	for i := range c.Instances {
		in := &c.Instances[i]
		targets, err := c.Targets(in)
		if err != nil {
			fmt.Fprintf(w, "  %s\t%s\t%s\tinvalid: %v\n", in.Name, in.Database, in.Bucket, err)
			continue
		}
		for j, t := range targets {
			name, db, bucket := in.Name, in.Database, in.Bucket
			if j > 0 {
				// Continuation rows: the instance columns stay blank so the
				// grouping is readable at a glance.
				name, db, bucket = "", "", ""
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s -> %s %v\n", name, db, bucket,
				t.Name, t.Repo, t.Retention.Args())
		}
	}
	return w.Flush()
}

func selectInstances(c *cfg.Config, name string) ([]*cfg.Instance, error) {
	if name != "" {
		in, err := c.Instance(name)
		if err != nil {
			return nil, err
		}
		return []*cfg.Instance{in}, nil
	}
	out := make([]*cfg.Instance, 0, len(c.Instances))
	for i := range c.Instances {
		out = append(out, &c.Instances[i])
	}
	return out, nil
}

func byteCount(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
