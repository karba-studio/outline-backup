package cfg

import (
	"testing"
	"time"
)

func TestParseHHMM(t *testing.T) {
	good := map[string][2]int{
		"03:00": {3, 0},
		"23:59": {23, 59},
		"0:05":  {0, 5},
		" 7:30": {7, 30},
	}
	for in, want := range good {
		h, m, err := ParseHHMM(in)
		if err != nil {
			t.Errorf("ParseHHMM(%q) errored: %v", in, err)
			continue
		}
		if h != want[0] || m != want[1] {
			t.Errorf("ParseHHMM(%q) = %d:%d, want %d:%d", in, h, m, want[0], want[1])
		}
	}
	for _, in := range []string{"24:00", "03:60", "noon", "", "3", "-1:00"} {
		if _, _, err := ParseHHMM(in); err == nil {
			t.Errorf("ParseHHMM(%q) was accepted", in)
		}
	}
}

func TestNextRunDaily(t *testing.T) {
	s := Schedule{Mode: "daily", Times: []string{"03:00", "15:00"}, Timezone: "UTC"}

	// Before both times: the earlier one today.
	now := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	if got := s.NextRun(now); !got.Equal(time.Date(2026, 9, 17, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("at 01:00 next run = %s, want 03:00 same day", got)
	}
	// Between them: the later one today.
	now = time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	if got := s.NextRun(now); !got.Equal(time.Date(2026, 9, 17, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("at 09:00 next run = %s, want 15:00 same day", got)
	}
	// After both: tomorrow's first.
	now = time.Date(2026, 9, 17, 20, 0, 0, 0, time.UTC)
	if got := s.NextRun(now); !got.Equal(time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("at 20:00 next run = %s, want 03:00 next day", got)
	}
	// Exactly at a scheduled time must move on, or the agent would loop.
	now = time.Date(2026, 9, 17, 3, 0, 0, 0, time.UTC)
	if got := s.NextRun(now); !got.After(now) {
		t.Errorf("at exactly 03:00 next run = %s, must be strictly later", got)
	}
}

func TestNextRunWeeklyAndMonthly(t *testing.T) {
	// 2026-09-17 is a Thursday.
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	weekly := Schedule{Mode: "weekly", Times: []string{"04:00"}, DayOfWeek: "sunday", Timezone: "UTC"}
	got := weekly.NextRun(now)
	if got.Weekday() != time.Sunday || got.Day() != 20 {
		t.Errorf("weekly next run = %s, want Sunday 2026-09-20 04:00", got)
	}

	// "every Saturday at 08:00 UTC", spelled out because it is the shape people
	// describe in words and then have to express in config.
	saturday := Schedule{Mode: "weekly", DayOfWeek: "saturday", Times: []string{"08:00"}, Timezone: "UTC"}
	got = saturday.NextRun(now)
	if got.Weekday() != time.Saturday || got.Hour() != 8 || got.Day() != 19 {
		t.Errorf("saturday next run = %s, want Sat 2026-09-19 08:00 UTC", got)
	}

	monthly := Schedule{Mode: "monthly", Times: []string{"05:00"}, DayOfMonth: 1, Timezone: "UTC"}
	got = monthly.NextRun(now)
	if got.Day() != 1 || got.Month() != time.October {
		t.Errorf("monthly next run = %s, want 2026-10-01 05:00", got)
	}

	manual := Schedule{Mode: "manual"}
	if got := manual.NextRun(now); !got.IsZero() {
		t.Errorf("manual schedule produced a run time %s", got)
	}
}

func TestRetentionArgs(t *testing.T) {
	r := Retention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
	want := []string{"--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "6"}
	got := r.Args()
	if len(got) != len(want) {
		t.Fatalf("Args() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Args() = %v, want %v", got, want)
		}
	}
	if (Retention{}).Empty() != true {
		t.Error("zero Retention should report Empty")
	}
	if r.Empty() {
		t.Error("populated Retention should not report Empty")
	}
}

func TestDestinationRepoNamespacesByInstance(t *testing.T) {
	cases := []struct {
		dest     Destination
		instance string
		want     string
	}{
		{Destination{Kind: "b2", Bucket: "karba-kb"}, "private", "b2:karba-kb:private"},
		{Destination{Kind: "b2", Bucket: "shared", Prefix: "/wiki/"}, "business", "b2:shared:wiki/business"},
		{Destination{Kind: "s3", Endpoint: "https://s3.example.com", Bucket: "b"}, "private", "s3:s3.example.com/b/private"},
		{Destination{Kind: "sftp", Path: "user@host:/srv/backups/"}, "private", "sftp:user@host:/srv/backups/private"},
		{Destination{Kind: "rclone", Path: "gdrive:backups/outline"}, "private", "rclone:gdrive:backups/outline/private"},
	}
	for _, tc := range cases {
		got, err := tc.dest.Repo(tc.instance)
		if err != nil {
			t.Errorf("Repo(%q) for %+v errored: %v", tc.instance, tc.dest, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Repo(%q) = %q, want %q", tc.instance, got, tc.want)
		}
	}
	for _, d := range []Destination{{Kind: "b2"}, {Kind: "nope", Bucket: "x"}, {Kind: "local"}, {Kind: "rclone"}} {
		if _, err := d.Repo("x"); err == nil {
			t.Errorf("Repo accepted invalid destination %+v", d)
		}
	}
}

// base returns a config with two instances and two destinations, which is the
// shape every test below varies.
func base() *Config {
	return &Config{
		Version:  SchemaVersion,
		Docker:   Docker{PostgresService: "postgres", PostgresSuperuser: "postgres"},
		Storage:  Storage{Endpoint: "https://s3.example.com"},
		Schedule: Schedule{Mode: "daily", Times: []string{"03:00"}},
		Destinations: map[string]Destination{
			"b2-one": {Kind: "b2", Bucket: "one", KeyIDEnv: "K1", AppKeyEnv: "A1", PasswordEnv: "P1"},
			"b2-two": {Kind: "b2", Bucket: "two", KeyIDEnv: "K2", AppKeyEnv: "A2", PasswordEnv: "P2"},
			"nas":    {Kind: "local", Path: "/mnt/nas", PasswordEnv: "P3"},
		},
		Instances: []Instance{
			{
				Name: "private", Database: "outline_private", Bucket: "outline-private",
				S3AccessKeyEnv: "A", S3SecretKeyEnv: "B", BackupTo: []string{"b2-one"},
			},
			{
				Name: "business", Database: "outline_business", Bucket: "outline-business",
				S3AccessKeyEnv: "C", S3SecretKeyEnv: "D", BackupTo: []string{"b2-two"},
			},
		},
	}
}

// One wiki, several destinations: the 3-2-1 rule expressed in config.
func TestOneInstanceToSeveralDestinations(t *testing.T) {
	c := base()
	c.Instances[0].BackupTo = []string{"b2-one", "b2-two", "nas"}
	if err := c.Validate(); err != nil {
		t.Fatalf("one instance with three destinations should be valid: %v", err)
	}
	targets, err := c.Targets(&c.Instances[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("got %d targets, want 3", len(targets))
	}
	want := []string{"b2:one:private", "b2:two:private", "/mnt/nas/private"}
	for i, t2 := range targets {
		if t2.Repo != want[i] {
			t.Errorf("target %d repo = %q, want %q", i, t2.Repo, want[i])
		}
	}
}

// Several wikis, one destination: allowed, because repositories are namespaced
// by instance and therefore never collide.
func TestSeveralInstancesShareOneDestination(t *testing.T) {
	c := base()
	c.Instances[0].BackupTo = []string{"nas"}
	c.Instances[1].BackupTo = []string{"nas"}
	if err := c.Validate(); err != nil {
		t.Fatalf("two instances sharing one destination should be valid: %v", err)
	}

	a, err := c.Targets(&c.Instances[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Targets(&c.Instances[1])
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Repo == b[0].Repo {
		t.Errorf("both instances resolved to %s; repositories must stay separate", a[0].Repo)
	}
}

// Two differently named destinations pointing at the same place would make
// restic interleave snapshots and let one retention policy expire the other's.
func TestValidateRejectsAliasedDestinations(t *testing.T) {
	c := base()
	c.Destinations["b2-dup"] = Destination{
		Kind: "b2", Bucket: "one", KeyIDEnv: "K1", AppKeyEnv: "A1", PasswordEnv: "P1",
	}
	c.Instances[0].BackupTo = []string{"b2-one", "b2-dup"}
	if err := c.Validate(); err == nil {
		t.Fatal("two destinations resolving to the same repository were accepted")
	}

	// Giving one of them a prefix separates them again.
	d := c.Destinations["b2-dup"]
	d.Prefix = "second-copy"
	c.Destinations["b2-dup"] = d
	if err := c.Validate(); err != nil {
		t.Fatalf("separating by prefix should be valid: %v", err)
	}
}

// A destination may shorten history for itself: a cheap local disk rarely wants
// the same retention as the offsite copy.
func TestDestinationRetentionOverridesInstance(t *testing.T) {
	c := base()
	c.Instances[0].Retention = Retention{KeepDaily: 30}
	c.Instances[0].BackupTo = []string{"b2-one", "nas"}
	nas := c.Destinations["nas"]
	nas.Retention = &Retention{KeepDaily: 3}
	c.Destinations["nas"] = nas

	targets, err := c.Targets(&c.Instances[0])
	if err != nil {
		t.Fatal(err)
	}
	if targets[0].Retention.KeepDaily != 30 {
		t.Errorf("b2 target keepDaily = %d, want the instance's 30", targets[0].Retention.KeepDaily)
	}
	if targets[1].Retention.KeepDaily != 3 {
		t.Errorf("nas target keepDaily = %d, want the destination's 3", targets[1].Retention.KeepDaily)
	}
}

func TestTargetSelection(t *testing.T) {
	c := base()
	c.Instances[0].BackupTo = []string{"b2-one", "nas"}

	// With several destinations, restoring must not silently pick one.
	if _, err := c.Target(&c.Instances[0], ""); err == nil {
		t.Error("ambiguous restore source was resolved silently")
	}
	got, err := c.Target(&c.Instances[0], "nas")
	if err != nil || got.Name != "nas" {
		t.Errorf("Target(nas) = %+v, %v", got, err)
	}
	if _, err := c.Target(&c.Instances[0], "nowhere"); err == nil {
		t.Error("unknown destination name was accepted")
	}

	// With exactly one, there is nothing to choose.
	c.Instances[1].BackupTo = []string{"b2-two"}
	if got, err := c.Target(&c.Instances[1], ""); err != nil || got.Name != "b2-two" {
		t.Errorf("single-destination instance should resolve without a flag: %+v %v", got, err)
	}
}

func TestValidateCatchesCommonMistakes(t *testing.T) {
	if err := base().Validate(); err != nil {
		t.Fatalf("baseline config should validate, got: %v", err)
	}

	c := base()
	c.Storage.Endpoint = "s3.example.com" // no scheme
	if err := c.Validate(); err == nil {
		t.Error("endpoint without a scheme was accepted")
	}

	c = base()
	c.Instances[0].Name = "Not Valid"
	if err := c.Validate(); err == nil {
		t.Error("instance name with spaces and capitals was accepted")
	}

	for _, name := range []string{"x", "a1", "kb-private", "wiki2"} {
		c = base()
		c.Instances[0].Name = name
		if err := c.Validate(); err != nil {
			t.Errorf("name %q should be valid: %v", name, err)
		}
	}
	for _, name := range []string{"", "-lead", "trail-", "UPPER", "has space", "dot.name"} {
		c = base()
		c.Instances[0].Name = name
		if err := c.Validate(); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}

	c = base()
	c.Instances[0].BackupTo = nil
	if err := c.Validate(); err == nil {
		t.Error("instance with no destinations was accepted; it would never be backed up")
	}

	c = base()
	c.Instances[0].BackupTo = []string{"does-not-exist"}
	if err := c.Validate(); err == nil {
		t.Error("reference to an undefined destination was accepted")
	}

	c = base()
	c.Schedule = Schedule{Mode: "monthly", Times: []string{"03:00"}, DayOfMonth: 31}
	if err := c.Validate(); err == nil {
		t.Error("dayOfMonth 31 was accepted; it does not exist in February")
	}

	c = base()
	d := c.Destinations["b2-one"]
	d.PasswordEnv = ""
	c.Destinations["b2-one"] = d
	if err := c.Validate(); err == nil {
		t.Error("destination without an encryption password was accepted")
	}
}
