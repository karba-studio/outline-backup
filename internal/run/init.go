package run

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/karba-studio/outline-backup/internal/cfg"
	"github.com/karba-studio/outline-backup/internal/dockerx"
	"github.com/karba-studio/outline-backup/internal/ui"
)

// Init walks the operator through building a configuration, discovering as much
// as it can from the running stack so the answers are mostly "press enter".
func Init(ctx context.Context, configPath string, out io.Writer) error {
	p := ui.New()
	p.Section("outline-backup: setup")
	p.Info("This asks for everything needed to back up each Outline instance and")
	p.Info("to restore it onto a different machine later. Defaults in [brackets]")
	p.Info("come from your running stack, so mostly you can press enter.")

	if _, err := os.Stat(configPath); err == nil {
		overwrite, err := p.Confirm(fmt.Sprintf("%s already exists. Replace it?", configPath), false)
		if err != nil {
			return err
		}
		if !overwrite {
			return fmt.Errorf("leaving the existing configuration alone")
		}
	}

	c := &cfg.Config{Version: cfg.SchemaVersion}
	secrets := map[string]string{}

	// ---- Docker ------------------------------------------------------------
	p.Section("1. The stack")

	docker := dockerx.New("", "")
	if err := docker.Available(ctx); err != nil {
		p.Warn("%v", err)
		p.Info("This tool reaches Postgres through its container, so docker must work here.")
		return err
	}

	projects, err := docker.Projects(ctx)
	if err != nil || len(projects) == 0 {
		p.Warn("could not list compose projects; you will have to type the name")
		c.Docker.ComposeProject, err = p.AskRequired("compose project name")
		if err != nil {
			return err
		}
	} else {
		labels := make([]string, len(projects))
		for i, pr := range projects {
			labels[i] = fmt.Sprintf("%s (%s)", pr.Name, pr.Status)
		}
		idx, err := p.Choose("Which compose project runs Outline?", labels, 0)
		if err != nil {
			return err
		}
		c.Docker.ComposeProject = projects[idx].Name
		// The config file path lets docker find services even when this tool
		// runs from a different working directory, which a scheduled task does.
		if cf := firstPath(projects[idx].ConfigFiles); cf != "" {
			keep, err := p.Confirm(fmt.Sprintf("Use compose file %s?", cf), true)
			if err != nil {
				return err
			}
			if keep {
				c.Docker.ComposeFile = cf
			}
		}
	}

	docker = dockerx.New(c.Docker.ComposeProject, c.Docker.ComposeFile)
	services, err := docker.Services(ctx)
	if err != nil {
		return fmt.Errorf("listing services of project %q: %w", c.Docker.ComposeProject, err)
	}
	p.Info("services: %s", strings.Join(services, ", "))

	c.Docker.PostgresService, err = p.Ask("Which service is Postgres?", guess(services, "postgres", "db"))
	if err != nil {
		return err
	}
	c.Docker.PostgresSuperuser, err = p.Ask("Postgres superuser", "postgres")
	if err != nil {
		return err
	}
	c.Docker.Network = c.Docker.ComposeProject + "_default"

	// ---- Instances ---------------------------------------------------------
	p.Section("2. Outline instances")
	p.Info("Each instance gets its own backup destination, so a compromise of one")
	p.Info("cannot reach the other's history.")

	candidates := filterServices(services, "outline")
	if len(candidates) == 0 {
		candidates = services
	}

	for _, svc := range candidates {
		include, err := p.Confirm(fmt.Sprintf("Back up service %q?", svc), strings.Contains(svc, "outline"))
		if err != nil {
			return err
		}
		if !include {
			continue
		}
		in, sec, err := askInstance(ctx, p, docker, svc, c)
		if err != nil {
			return err
		}
		c.Instances = append(c.Instances, *in)
		for k, v := range sec {
			secrets[k] = v
		}
	}
	if len(c.Instances) == 0 {
		return fmt.Errorf("no instances selected; nothing to back up")
	}

	// ---- Object store -------------------------------------------------------------
	p.Section("3. Object store (S3 API)")
	p.Info("This must be an address reachable from wherever backups run.")
	c.Storage.Endpoint, err = p.Ask("S3 endpoint", c.Storage.Endpoint)
	if err != nil {
		return err
	}
	c.Storage.Region, err = p.Ask("region", firstNonEmpty(c.Storage.Region, "us-east-1"))
	if err != nil {
		return err
	}
	c.Storage.PathStyle = true

	// ---- Destinations ------------------------------------------------------
	p.Section("4. Where backups go")
	p.Info("Destinations are defined once here and then assigned to instances, so")
	p.Info("one wiki can be copied to several places (a second provider, a NAS),")
	p.Info("and several wikis can share one place.")

	c.Destinations = map[string]cfg.Destination{}
	for n := 1; ; n++ {
		add := true
		if n > 1 {
			if add, err = p.Confirm("Add another destination?", false); err != nil {
				return err
			}
		}
		if !add {
			break
		}
		name, dest, sec, err := askDestination(p, n)
		if err != nil {
			return err
		}
		c.Destinations[name] = dest
		for k, v := range sec {
			secrets[k] = v
		}
	}
	if len(c.Destinations) == 0 {
		return fmt.Errorf("no destinations defined; there would be nowhere to put backups")
	}

	// ---- Assignment --------------------------------------------------------
	names := make([]string, 0, len(c.Destinations))
	for k := range c.Destinations {
		names = append(names, k)
	}
	sort.Strings(names)

	if len(c.Destinations) == 1 {
		// Nothing to choose; assign the only one.
		for i := range c.Instances {
			c.Instances[i].BackupTo = []string{names[0]}
		}
	} else {
		p.Section("5. Which destination for which instance")
		p.Info("available: %s", strings.Join(names, ", "))
		for i := range c.Instances {
			for {
				answer, err := p.Ask(
					fmt.Sprintf("  destinations for %q (comma-separated)", c.Instances[i].Name),
					strings.Join(names, ","))
				if err != nil {
					return err
				}
				chosen, bad := splitKnown(answer, names)
				if len(bad) > 0 {
					p.Warn("unknown: %s", strings.Join(bad, ", "))
					continue
				}
				if len(chosen) == 0 {
					p.Warn("pick at least one, or this instance is never backed up")
					continue
				}
				c.Instances[i].BackupTo = chosen
				break
			}
		}
	}

	// ---- Schedule ----------------------------------------------------------
	p.Section("6. Schedule")
	modes := []string{
		"daily            - one or more times every day",
		"weekly           - on one weekday",
		"monthly          - on one day of the month",
		"manual           - only when you run it",
	}
	idx, err := p.Choose("How often should backups run?", modes, 0)
	if err != nil {
		return err
	}
	c.Schedule.Mode = []string{"daily", "weekly", "monthly", "manual"}[idx]
	if c.Schedule.Mode != "manual" {
		times, err := p.Ask("at what time(s)? comma-separated HH:MM", "03:00")
		if err != nil {
			return err
		}
		for _, t := range strings.Split(times, ",") {
			if s := strings.TrimSpace(t); s != "" {
				c.Schedule.Times = append(c.Schedule.Times, s)
			}
		}
		switch c.Schedule.Mode {
		case "weekly":
			if c.Schedule.DayOfWeek, err = p.Ask("which weekday", "sunday"); err != nil {
				return err
			}
		case "monthly":
			if c.Schedule.DayOfMonth, err = p.AskInt("which day of the month (1-28)", 1, 1, 28); err != nil {
				return err
			}
		}
		if c.Schedule.Timezone, err = p.Ask("timezone (IANA name, empty for this machine's local time)", ""); err != nil {
			return err
		}
	}

	// ---- Retention ---------------------------------------------------------
	p.Section("7. How much history to keep")
	p.Info("Snapshots are deduplicated, so keeping more history costs far less")
	p.Info("than it looks. These apply per instance.")
	var ret cfg.Retention
	p.Info("")
	p.Info("The simplest rule is a plain cap: keep the last N backups and drop the")
	p.Info("rest. Set it to 0 to use the calendar rules below instead.")
	if ret.KeepLast, err = p.AskInt("keep at most this many backups (0 = no cap)", 0, 0, 10000); err != nil {
		return err
	}
	if ret.KeepLast > 0 {
		p.Info("")
		p.Info("Calendar rules can be combined with the cap; leave them at 0 to keep")
		p.Info("exactly %d and nothing else.", ret.KeepLast)
	}
	if ret.KeepDaily, err = p.AskInt("keep this many daily snapshots", 7, 0, 3650); err != nil {
		return err
	}
	if ret.KeepWeekly, err = p.AskInt("keep this many weekly", 4, 0, 520); err != nil {
		return err
	}
	if ret.KeepMonthly, err = p.AskInt("keep this many monthly", 6, 0, 240); err != nil {
		return err
	}
	if ret.KeepYearly, err = p.AskInt("keep this many yearly", 2, 0, 100); err != nil {
		return err
	}
	for i := range c.Instances {
		c.Instances[i].Retention = ret
	}

	// ---- Write -------------------------------------------------------------
	p.Section("8. Writing configuration")
	c.StagingDir = filepath.Join(filepath.Dir(configPath), "staging")
	if err := c.Validate(); err != nil {
		return err
	}
	if err := c.Save(configPath); err != nil {
		return err
	}
	secretsPath := filepath.Join(filepath.Dir(configPath), cfg.DefaultSecretsName)
	if err := cfg.SaveSecrets(secretsPath, secrets); err != nil {
		return err
	}
	p.Info("config  : %s", configPath)
	p.Info("secrets : %s  (owner-readable only)", secretsPath)

	p.Section("Before you go")
	p.Warn("Save every *_RESTIC_PASSWORD from the secrets file into your password manager.")
	p.Warn("Without it, these backups cannot be decrypted by anyone, including you.")
	p.Info("")
	p.Info("Next: outline-backup check    (verifies every part, writes nothing)")
	p.Info("      outline-backup backup   (first real run)")

	return nil
}

// askInstance fills in one instance, reading what it can out of the container.
func askInstance(ctx context.Context, p *ui.Prompter, docker *dockerx.Client, service string,
	c *cfg.Config) (*cfg.Instance, map[string]string, error) {

	secrets := map[string]string{}
	in := &cfg.Instance{}

	env, err := docker.ContainerEnv(ctx, service)
	if err != nil {
		p.Warn("could not read %s's environment (%v); asking instead", service, err)
		env = map[string]string{}
	}

	defName := strings.TrimPrefix(service, "outline-")
	if in.Name, err = p.Ask("  short name for this instance", defName); err != nil {
		return nil, nil, err
	}
	if in.Description, err = p.Ask("  description", env["URL"]); err != nil {
		return nil, nil, err
	}
	if in.Database, err = p.Ask("  database name", databaseFromURL(env["DATABASE_URL"])); err != nil {
		return nil, nil, err
	}
	if in.Bucket, err = p.Ask("  bucket holding this instance's attachments", env["AWS_S3_UPLOAD_BUCKET_NAME"]); err != nil {
		return nil, nil, err
	}

	// The endpoint is a property of the stack, not the instance, but the
	// container is where it is discoverable.
	if c.Storage.Endpoint == "" {
		c.Storage.Endpoint = env["AWS_S3_UPLOAD_BUCKET_URL"]
	}
	if c.Storage.Region == "" {
		c.Storage.Region = env["AWS_REGION"]
	}

	prefix := "OMB_" + strings.ToUpper(strings.ReplaceAll(in.Name, "-", "_"))
	in.S3AccessKeyEnv = prefix + "_S3_KEY"
	in.S3SecretKeyEnv = prefix + "_S3_SECRET"

	ak, sk := env["AWS_ACCESS_KEY_ID"], env["AWS_SECRET_ACCESS_KEY"]
	if ak != "" && sk != "" {
		p.Info("  found this instance's object-store credentials in the container")
	} else {
		if ak, err = p.AskRequired("  access key"); err != nil {
			return nil, nil, err
		}
		if sk, err = p.AskSecret("  secret key"); err != nil {
			return nil, nil, err
		}
	}
	secrets[in.S3AccessKeyEnv] = ak
	secrets[in.S3SecretKeyEnv] = sk

	includeKC, err := p.Confirm("  include the Keycloak database in this backup?", true)
	if err != nil {
		return nil, nil, err
	}
	if includeKC {
		p.Info("  (it is shared by both wikis; copying it into each destination is a few")
		p.Info("   hundred kB and makes either one restorable on its own)")
		if in.KeycloakDatabase, err = p.Ask("  keycloak database name", "keycloak"); err != nil {
			return nil, nil, err
		}
	}

	p.Info("  Outline encrypts some stored tokens with SECRET_KEY, so including .env")
	p.Info("  makes a restore seamless rather than merely possible.")
	if in.EnvFile, err = p.Ask("  path to the stack .env (empty to skip)", ""); err != nil {
		return nil, nil, err
	}
	if in.EnvFile != "" {
		if _, err := os.Stat(in.EnvFile); err != nil {
			p.Warn("  %v - continuing, but the backup will fail until this path is right", err)
		}
	}

	return in, secrets, nil
}

// askDestination defines one named destination and the secrets it needs.
func askDestination(p *ui.Prompter, n int) (string, cfg.Destination, map[string]string, error) {
	secrets := map[string]string{}
	var d cfg.Destination

	name, err := p.AskRequired(fmt.Sprintf("  name for destination %d (e.g. b2-private, nas)", n))
	if err != nil {
		return "", d, nil, err
	}
	prefix := "OMB_DEST_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))

	kinds := []string{
		"b2     - Backblaze B2",
		"s3     - any S3-compatible service",
		"sftp   - an SSH server",
		"local  - a directory or mounted drive",
		"rclone - anything rclone supports (Google Drive, OneDrive, Dropbox, ...)",
	}
	kidx, err := p.Choose("  what kind of storage is it?", kinds, 0)
	if err != nil {
		return "", d, nil, err
	}
	d.Kind = []string{"b2", "s3", "sftp", "local", "rclone"}[kidx]

	switch d.Kind {
	case "b2", "s3":
		if d.Kind == "s3" {
			if d.Endpoint, err = p.AskRequired("  endpoint URL"); err != nil {
				return "", d, nil, err
			}
		}
		if d.Bucket, err = p.AskRequired("  bucket name"); err != nil {
			return "", d, nil, err
		}
		if d.Prefix, err = p.Ask("  path inside the bucket (optional)", ""); err != nil {
			return "", d, nil, err
		}
		d.KeyIDEnv = prefix + "_KEY_ID"
		d.AppKeyEnv = prefix + "_APP_KEY"
		keyID, err := p.AskRequired("  application key ID")
		if err != nil {
			return "", d, nil, err
		}
		appKey, err := p.AskSecret("  application key")
		if err != nil {
			return "", d, nil, err
		}
		secrets[d.KeyIDEnv] = keyID
		secrets[d.AppKeyEnv] = appKey
	case "sftp", "local":
		if d.Path, err = p.AskRequired("  path"); err != nil {
			return "", d, nil, err
		}
	case "rclone":
		p.Info("  Configure the remote with `rclone config` first; give its name here.")
		if d.Path, err = p.AskRequired("  rclone remote and path (e.g. gdrive:backups/outline)"); err != nil {
			return "", d, nil, err
		}
	}

	d.PasswordEnv = prefix + "_PASSWORD"
	p.Info("")
	p.Info("  Everything sent here is encrypted on this machine before it leaves,")
	p.Info("  so the storage provider only ever holds ciphertext.")
	generate, err := p.Confirm("  generate the encryption password?", true)
	if err != nil {
		return "", d, nil, err
	}
	var password string
	if generate {
		if password, err = ui.GeneratePassword(32); err != nil {
			return "", d, nil, err
		}
		p.Info("")
		p.Warn("Encryption password for destination %q — copy it somewhere safe now:", name)
		p.Info("      %s", password)
		p.Warn("If this is lost, these backups are permanently unreadable. There is no")
		p.Warn("recovery path; that is what makes the encryption worth having.")
		if _, err := p.Ask("  press enter once you have saved it", ""); err != nil {
			return "", d, nil, err
		}
	} else {
		if password, err = p.AskSecret("  encryption password"); err != nil {
			return "", d, nil, err
		}
		if len(password) < 12 {
			p.Warn("  that is quite short for something protecting an offsite copy")
		}
	}
	secrets[d.PasswordEnv] = password

	return name, d, secrets, nil
}

// splitKnown parses a comma-separated answer, returning the recognised entries
// and anything that did not match.
func splitKnown(answer string, known []string) (chosen, unknown []string) {
	seen := map[string]bool{}
	for _, raw := range strings.Split(answer, ",") {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		ok := false
		for _, k := range known {
			if k == v {
				ok = true
				break
			}
		}
		switch {
		case !ok:
			unknown = append(unknown, v)
		case !seen[v]:
			seen[v] = true
			chosen = append(chosen, v)
		}
	}
	return chosen, unknown
}

func guess(services []string, needles ...string) string {
	for _, n := range needles {
		for _, s := range services {
			if strings.Contains(s, n) {
				return s
			}
		}
	}
	if len(services) > 0 {
		return services[0]
	}
	return ""
}

func filterServices(services []string, needle string) []string {
	var out []string
	for _, s := range services {
		if strings.Contains(s, needle) {
			out = append(out, s)
		}
	}
	return out
}

// databaseFromURL pulls the database name out of a libpq connection URL.
func databaseFromURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

func firstPath(list string) string {
	for _, p := range strings.Split(list, ",") {
		if s := strings.TrimSpace(p); s != "" {
			return s
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
