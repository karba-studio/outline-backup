// Package cfg defines the on-disk configuration and its secrets sidecar.
//
// Two files live side by side:
//
//	config.json   - structure only, no secrets. Safe to commit.
//	secrets.env   - KEY=value lines, chmod 0600. Never commit.
//
// config.json refers to secrets by environment variable NAME, never by value,
// so a leaked config tells an attacker the shape of the setup and nothing more.
package cfg

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SchemaVersion is bumped when a migration is needed to read older files.
const SchemaVersion = 1

// DefaultConfigName and DefaultSecretsName are the file names used inside the
// configuration directory.
const (
	DefaultConfigName  = "config.json"
	DefaultSecretsName = "secrets.env"
)

// Config is the whole tool configuration.
type Config struct {
	Version int    `json:"version"`
	Docker  Docker `json:"docker"`
	// Storage is the default object store for every instance. One MinIO
	// typically serves several Outline instances, each with its own bucket, so
	// the endpoint belongs here rather than on the instance.
	Storage Storage `json:"storage"`
	// Destinations are declared once by name and referenced by instances, so one
	// wiki can be copied to several places and several wikis can share a place.
	Destinations map[string]Destination `json:"destinations"`
	Instances    []Instance             `json:"instances"`
	Schedule     Schedule               `json:"schedule"`
	// StagingDir holds the working copy assembled before it is handed to the
	// backup engine. Emptied after every run. Defaults next to the config.
	StagingDir string `json:"stagingDir,omitempty"`

	// path is where this config was loaded from; not serialised.
	path string
}

// Docker describes how to reach the running Outline stack.
type Docker struct {
	// ComposeProject is the `name:` in docker-compose.yml (e.g. "outline").
	ComposeProject string `json:"composeProject"`
	// ComposeFile is optional; when set, commands run with `-f <file>`.
	ComposeFile string `json:"composeFile,omitempty"`
	// Network is the compose network used for one-off helper containers.
	Network string `json:"network"`
	// PostgresService is the compose service name of the database.
	PostgresService string `json:"postgresService"`
	// PostgresSuperuser is the role used for dump and restore.
	PostgresSuperuser string `json:"postgresSuperuser"`
}

// Storage describes an S3-compatible endpoint holding Outline's attachments.
//
// Nothing here is MinIO-specific: the tool speaks plain S3, so the same
// configuration works against MinIO, AWS S3, Wasabi, Cloudflare R2, Garage or
// anything else that implements the API.
type Storage struct {
	// Endpoint is the base URL, e.g. https://s3.example.com.
	// It must be reachable from wherever this tool runs.
	Endpoint string `json:"endpoint"`
	Region   string `json:"region,omitempty"`
	// PathStyle addresses buckets as <endpoint>/<bucket>. MinIO and most
	// self-hosted implementations require it; AWS S3 does not.
	PathStyle bool `json:"pathStyle"`
}

// merge returns o with any empty field filled in from the default d.
func (o Storage) merge(d Storage) Storage {
	if o.Endpoint == "" {
		o.Endpoint = d.Endpoint
		// PathStyle is a bool, so it cannot signal "unset" on its own. It is
		// only inherited when the endpoint is too, i.e. when the override says
		// nothing about where the storage lives.
		o.PathStyle = d.PathStyle
	}
	if o.Region == "" {
		o.Region = d.Region
	}
	if o.Region == "" {
		o.Region = "us-east-1"
	}
	return o
}

// StorageFor resolves the endpoint an instance's attachments live on: its own
// override when it has one, otherwise the shared default.
func (c *Config) StorageFor(in *Instance) Storage {
	if in != nil && in.Storage != nil {
		return in.Storage.merge(c.Storage)
	}
	return c.Storage.merge(Storage{})
}

// Instance is one Outline deployment: its database, its bucket, and where its
// backups go. Two instances sharing a host still back up to separate
// destinations, so a compromise of one cannot reach the other's history.
type Instance struct {
	// Name is the short identifier used in paths and on the command line.
	Name string `json:"name"`
	// Description is free text, e.g. the public URL.
	Description string `json:"description,omitempty"`
	// Database is the Postgres database name for this instance.
	Database string `json:"database"`
	// Bucket holds this instance's attachments on the object store.
	Bucket string `json:"bucket"`
	// Storage overrides the shared endpoint for this instance alone. Normally
	// nil: several Outline instances usually share one object store and differ
	// only by bucket.
	Storage *Storage `json:"storage,omitempty"`
	// S3AccessKeyEnv / S3SecretKeyEnv name the env vars holding this instance's
	// object-store credentials. Each instance gets a key scoped to its own
	// bucket, so one leaked key cannot read another wiki's attachments.
	S3AccessKeyEnv string `json:"s3AccessKeyEnv"`
	S3SecretKeyEnv string `json:"s3SecretKeyEnv"`
	// KeycloakDatabase, when set, is dumped into this instance's backup too.
	// Both instances normally share one Keycloak database; duplicating it is a
	// few hundred kB and makes each destination independently restorable.
	KeycloakDatabase string `json:"keycloakDatabase,omitempty"`
	// EnvFile is an optional path to the stack's .env, included in the backup.
	// Outline encrypts some stored tokens with SECRET_KEY; without it a restore
	// works but every integration must re-authenticate.
	EnvFile string `json:"envFile,omitempty"`
	// ExtraFiles are additional paths to include verbatim (compose file, etc).
	ExtraFiles []string `json:"extraFiles,omitempty"`

	// BackupTo names the destinations this instance is copied to, in order.
	// Several entries means several independent copies of the same snapshot -
	// the 3-2-1 rule expressed in config. Every named destination must exist in
	// the top-level "destinations" map.
	BackupTo []string `json:"backupTo"`
	// Retention applies to every destination unless that destination overrides
	// it (a cheap local disk usually wants a shorter history than the cloud).
	Retention Retention `json:"retention"`
}

// Destination is where an instance's encrypted snapshots are stored.
type Destination struct {
	// Kind is one of: b2, s3, sftp, local.
	Kind string `json:"kind"`
	// Bucket (b2/s3) or Path (local/sftp).
	Bucket string `json:"bucket,omitempty"`
	Path   string `json:"path,omitempty"`
	// Prefix nests the repository inside the bucket. Optional.
	Prefix string `json:"prefix,omitempty"`
	// Endpoint overrides the S3 endpoint for Kind=="s3".
	Endpoint string `json:"endpoint,omitempty"`

	// Credential env var names.
	KeyIDEnv  string `json:"keyIdEnv,omitempty"`
	AppKeyEnv string `json:"appKeyEnv,omitempty"`
	// PasswordEnv names the var holding the encryption password for repositories
	// in this destination. Losing it means losing those backups; there is no
	// recovery path.
	//
	// Instances sharing a destination share this password. When two wikis must
	// not be readable with one key, give them separate destination entries -
	// they may still point at the same bucket, with different prefixes.
	PasswordEnv string `json:"passwordEnv"`
	// Retention overrides the instance's policy for this destination only.
	Retention *Retention `json:"retention,omitempty"`
}

// Repo renders the restic repository URL for one instance in this destination.
//
// The instance name is always part of the path, so each (instance, destination)
// pair gets its own repository even when several wikis share one bucket. That
// matters for more than tidiness: a shared repository would interleave
// snapshots, and one instance's retention policy would expire another's
// history.
func (d Destination) Repo(instance string) (string, error) {
	seg := strings.Trim(strings.Trim(d.Prefix, "/")+"/"+instance, "/")

	switch d.Kind {
	case "b2":
		if d.Bucket == "" {
			return "", errors.New("bucket is required for kind=b2")
		}
		return "b2:" + d.Bucket + ":" + seg, nil
	case "s3":
		if d.Endpoint == "" || d.Bucket == "" {
			return "", errors.New("endpoint and bucket are required for kind=s3")
		}
		host := strings.TrimPrefix(strings.TrimPrefix(d.Endpoint, "https://"), "http://")
		return "s3:" + host + "/" + d.Bucket + "/" + seg, nil
	case "sftp":
		if d.Path == "" {
			return "", errors.New("path is required for kind=sftp")
		}
		return "sftp:" + strings.TrimRight(d.Path, "/") + "/" + instance, nil
	case "local":
		if d.Path == "" {
			return "", errors.New("path is required for kind=local")
		}
		return filepath.Join(d.Path, instance), nil
	case "rclone":
		// An escape hatch worth having: rclone speaks to around seventy
		// services, so anything it supports - Google Drive, OneDrive, Dropbox,
		// pCloud, Azure, a Hetzner box - becomes a destination without this
		// tool needing to know anything about it. Path is an rclone remote,
		// e.g. "gdrive:backups/outline"; configure it with `rclone config`.
		if d.Path == "" {
			return "", errors.New("path is required for kind=rclone (e.g. gdrive:backups)")
		}
		return "rclone:" + strings.TrimRight(d.Path, "/") + "/" + instance, nil
	default:
		return "", fmt.Errorf("unknown destination kind %q "+
			"(expected b2, s3, sftp, local or rclone)", d.Kind)
	}
}

// Target is one place one instance is backed up to: a resolved destination,
// the repository URL, and the retention policy that applies there.
type Target struct {
	// Name is the key from the destinations map, used on the command line.
	Name        string
	Destination Destination
	Repo        string
	Retention   Retention
}

// Targets resolves every destination an instance is copied to.
func (c *Config) Targets(in *Instance) ([]Target, error) {
	if len(in.BackupTo) == 0 {
		return nil, fmt.Errorf("instance %q has an empty backupTo", in.Name)
	}
	out := make([]Target, 0, len(in.BackupTo))
	for _, name := range in.BackupTo {
		d, ok := c.Destinations[name]
		if !ok {
			return nil, fmt.Errorf("instance %q refers to destination %q, which is not defined",
				in.Name, name)
		}
		repo, err := d.Repo(in.Name)
		if err != nil {
			return nil, fmt.Errorf("destination %q: %w", name, err)
		}
		ret := in.Retention
		if d.Retention != nil {
			ret = *d.Retention
		}
		out = append(out, Target{Name: name, Destination: d, Repo: repo, Retention: ret})
	}
	return out, nil
}

// Target finds one named destination for an instance.
func (c *Config) Target(in *Instance, name string) (*Target, error) {
	targets, err := c.Targets(in)
	if err != nil {
		return nil, err
	}
	if name == "" {
		// With one destination there is nothing to choose. With several,
		// guessing which copy to restore from is not a decision to make silently.
		if len(targets) == 1 {
			return &targets[0], nil
		}
		names := make([]string, len(targets))
		for i, t := range targets {
			names[i] = t.Name
		}
		return nil, fmt.Errorf("instance %q has %d destinations (%s); pick one with --destination",
			in.Name, len(targets), strings.Join(names, ", "))
	}
	for i := range targets {
		if targets[i].Name == name {
			return &targets[i], nil
		}
	}
	return nil, fmt.Errorf("instance %q does not back up to %q", in.Name, name)
}

// Retention is the snapshot expiry policy, passed through to `restic forget`.
// Zero values are omitted, so an all-zero policy keeps everything forever.
type Retention struct {
	KeepLast    int `json:"keepLast,omitempty"`
	KeepHourly  int `json:"keepHourly,omitempty"`
	KeepDaily   int `json:"keepDaily,omitempty"`
	KeepWeekly  int `json:"keepWeekly,omitempty"`
	KeepMonthly int `json:"keepMonthly,omitempty"`
	KeepYearly  int `json:"keepYearly,omitempty"`
}

// Empty reports whether the policy would delete nothing.
func (r Retention) Empty() bool {
	return r.KeepLast == 0 && r.KeepHourly == 0 && r.KeepDaily == 0 &&
		r.KeepWeekly == 0 && r.KeepMonthly == 0 && r.KeepYearly == 0
}

// Args renders the policy as restic flags.
func (r Retention) Args() []string {
	var a []string
	add := func(flag string, n int) {
		if n > 0 {
			a = append(a, flag, fmt.Sprint(n))
		}
	}
	add("--keep-last", r.KeepLast)
	add("--keep-hourly", r.KeepHourly)
	add("--keep-daily", r.KeepDaily)
	add("--keep-weekly", r.KeepWeekly)
	add("--keep-monthly", r.KeepMonthly)
	add("--keep-yearly", r.KeepYearly)
	return a
}

// Schedule describes when the agent runs backups.
type Schedule struct {
	// Mode: daily | weekly | monthly | manual
	Mode string `json:"mode"`
	// Times are "HH:MM" in Timezone. Several entries mean several runs a day.
	Times []string `json:"times"`
	// DayOfWeek is used when Mode=="weekly" (monday..sunday).
	DayOfWeek string `json:"dayOfWeek,omitempty"`
	// DayOfMonth is used when Mode=="monthly" (1..28; 28 is the safe maximum).
	DayOfMonth int `json:"dayOfMonth,omitempty"`
	// Timezone is an IANA name; empty means the host's local time.
	Timezone string `json:"timezone,omitempty"`
}

// Path returns where this config was loaded from.
func (c *Config) Path() string { return c.path }

// Dir returns the directory holding the config and its secrets sidecar.
func (c *Config) Dir() string { return filepath.Dir(c.path) }

// SecretsPath returns the expected location of the secrets sidecar.
func (c *Config) SecretsPath() string {
	return filepath.Join(c.Dir(), DefaultSecretsName)
}

// Staging returns the working directory for assembling backups.
func (c *Config) Staging() string {
	if c.StagingDir != "" {
		return c.StagingDir
	}
	return filepath.Join(c.Dir(), "staging")
}

// Instance looks up an instance by name.
func (c *Config) Instance(name string) (*Instance, error) {
	for i := range c.Instances {
		if c.Instances[i].Name == name {
			return &c.Instances[i], nil
		}
	}
	names := make([]string, 0, len(c.Instances))
	for _, in := range c.Instances {
		names = append(names, in.Name)
	}
	return nil, fmt.Errorf("no instance named %q (have: %s)", name, strings.Join(names, ", "))
}

// Names appear in repository paths and on the command line, so they are
// restricted in shape but not in length: a single character is fine.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Validate checks the config for the mistakes that would only surface halfway
// through a backup, when it is least welcome.
func (c *Config) Validate() error {
	var problems []string
	bad := func(format string, a ...any) {
		problems = append(problems, fmt.Sprintf(format, a...))
	}

	if c.Version != SchemaVersion {
		bad("version is %d, this build understands %d", c.Version, SchemaVersion)
	}
	if c.Docker.PostgresService == "" {
		bad("docker.postgresService is empty")
	}
	if c.Docker.PostgresSuperuser == "" {
		bad("docker.postgresSuperuser is empty")
	}
	if len(c.Instances) == 0 {
		bad("no instances configured")
	}

	seenName := map[string]bool{}
	seenRepo := map[string]string{}
	for i := range c.Instances {
		in := c.Instances[i]
		if !nameRe.MatchString(in.Name) {
			bad("instance name %q must be lowercase letters, digits and dashes", in.Name)
			continue
		}
		if seenName[in.Name] {
			bad("two instances are both named %q", in.Name)
		}
		seenName[in.Name] = true

		if in.Database == "" {
			bad("instance %q: database is empty", in.Name)
		}
		if in.Bucket == "" {
			bad("instance %q: bucket is empty", in.Name)
		}
		// Resolved per instance, because an instance may override the endpoint.
		switch ep := c.StorageFor(&c.Instances[i]).Endpoint; {
		case ep == "":
			bad("instance %q: no storage endpoint (set storage.endpoint)", in.Name)
		case !strings.HasPrefix(ep, "http://") && !strings.HasPrefix(ep, "https://"):
			bad("instance %q: storage endpoint %q must start with http:// or https://", in.Name, ep)
		}
		if in.S3AccessKeyEnv == "" || in.S3SecretKeyEnv == "" {
			bad("instance %q: object-store credential env vars are not set", in.Name)
		}
		if len(in.BackupTo) == 0 {
			bad("instance %q: backupTo is empty, so it is never backed up anywhere", in.Name)
		}
		targets, err := c.Targets(&c.Instances[i])
		if err != nil {
			bad("%v", err)
			continue
		}
		seenTarget := map[string]bool{}
		for _, t := range targets {
			if seenTarget[t.Name] {
				bad("instance %q lists destination %q twice", in.Name, t.Name)
			}
			seenTarget[t.Name] = true

			if t.Destination.PasswordEnv == "" {
				bad("destination %q: passwordEnv is empty, so nothing would be encrypted", t.Name)
			}
			// Repositories are namespaced by instance, so a collision here means
			// two destinations resolve to the same place - different names for
			// one bucket and prefix. restic would interleave their snapshots and
			// one retention policy would expire the other's history.
			if other, dup := seenRepo[t.Repo]; dup {
				bad("%s and %s both resolve to %s; give one of them a different prefix",
					other, in.Name+"/"+t.Name, t.Repo)
			}
			seenRepo[t.Repo] = in.Name + "/" + t.Name
		}
	}

	for name, d := range c.Destinations {
		if !nameRe.MatchString(name) {
			bad("destination name %q must be lowercase letters, digits and dashes", name)
		}
		if _, err := d.Repo("probe"); err != nil {
			bad("destination %q: %v", name, err)
		}
		if (d.Kind == "b2" || d.Kind == "s3") && (d.KeyIDEnv == "" || d.AppKeyEnv == "") {
			bad("destination %q: credential env vars are not set", name)
		}
	}

	switch c.Schedule.Mode {
	case "", "manual":
	case "daily", "weekly", "monthly":
		if len(c.Schedule.Times) == 0 {
			bad("schedule.times is empty for mode %q", c.Schedule.Mode)
		}
		for _, t := range c.Schedule.Times {
			if _, _, err := ParseHHMM(t); err != nil {
				bad("schedule.times: %v", err)
			}
		}
		if c.Schedule.Mode == "weekly" && WeekdayIndex(c.Schedule.DayOfWeek) < 0 {
			bad("schedule.dayOfWeek %q is not a weekday name", c.Schedule.DayOfWeek)
		}
		if c.Schedule.Mode == "monthly" && (c.Schedule.DayOfMonth < 1 || c.Schedule.DayOfMonth > 28) {
			bad("schedule.dayOfMonth must be 1..28 so it exists in February")
		}
	default:
		bad("schedule.mode %q is not one of daily, weekly, monthly, manual", c.Schedule.Mode)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("config is not usable:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// Load reads a config file. Comments are allowed: this file is meant to be
// edited by hand, and a retention policy with no explanation next to it is a
// retention policy nobody dares change.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(stripComments(b)))
	// Unknown fields are an error rather than a shrug: a typo in "keepDaily"
	// would otherwise silently mean "keep nothing on that schedule".
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	return &c, nil
}

// Save writes the config, pretty-printed so a human can diff it.
func (c *Config) Save(path string) error {
	if path == "" {
		path = c.path
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := writeFileAtomic(path, b, 0o644); err != nil {
		return err
	}
	c.path = path
	return nil
}

// LoadSecrets reads a KEY=value file into the process environment. Values
// already present in the environment win, so a container can override the file.
func LoadSecrets(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := warnIfWorldReadable(path); err != nil {
		return err
	}

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=value", path, line)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, present := os.LookupEnv(k); present {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return sc.Err()
}

// SaveSecrets writes the sidecar with owner-only permissions.
func SaveSecrets(path string, kv map[string]string) error {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("# outline-backup secrets. Keep private, never commit.\n")
	b.WriteString("# Losing a *_RESTIC_PASSWORD value makes those backups unreadable forever.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, kv[k])
	}
	return writeFileAtomic(path, []byte(b.String()), 0o600)
}

// MustEnv returns an env var's value or explains which one is missing.
func MustEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("environment variable %s is not set (expected in secrets.env)", name)
	}
	return v, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	// Windows will not rename onto an existing file.
	_ = os.Remove(path)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, perm)
}
