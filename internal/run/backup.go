// Package run holds the orchestration: what a backup, a restore and a health
// check actually do, in order.
package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/karba-studio/outline-backup/internal/cfg"
	"github.com/karba-studio/outline-backup/internal/dockerx"
	"github.com/karba-studio/outline-backup/internal/pg"
	"github.com/karba-studio/outline-backup/internal/resticx"
	"github.com/karba-studio/outline-backup/internal/s3"
	"github.com/karba-studio/outline-backup/internal/version"
)

// Layout inside a snapshot. Restore depends on these names, and so does anyone
// poking around a restored directory by hand, so they are deliberately obvious.
const (
	DirDatabase = "database"
	DirAssets   = "assets"
	DirStack    = "stack"
	ManifestTxt = "MANIFEST.txt"
)

// Dump file names inside DirDatabase. These are roles, not database names: the
// restore target decides what each one is called when it is loaded back, so a
// snapshot of "outline_business" can be restored into "outline_business_test"
// without the tool quietly writing to the production name it came from.
const (
	RoleInstance = "instance"
	RoleKeycloak = "keycloak"
)

// Copy is the outcome of shipping one snapshot to one destination.
type Copy struct {
	Destination string
	Repo        string
	SnapshotID  string
	Err         error
}

// Result summarises one instance's backup across every destination.
type Result struct {
	Instance   string
	Copies     []Copy
	DumpBytes  int64
	AssetFiles int
	AssetBytes int64
	Duration   time.Duration
}

// Succeeded reports how many destinations received the snapshot.
func (r *Result) Succeeded() int {
	n := 0
	for _, c := range r.Copies {
		if c.Err == nil {
			n++
		}
	}
	return n
}

// Backup runs a full backup for one instance.
func Backup(ctx context.Context, c *cfg.Config, in *cfg.Instance, out io.Writer) (*Result, error) {
	start := time.Now()
	logf := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }

	docker := dockerx.New(c.Docker.ComposeProject, c.Docker.ComposeFile)
	if err := docker.Available(ctx); err != nil {
		return nil, err
	}

	staging := filepath.Join(c.Staging(), in.Name)
	// A leftover staging directory from a crashed run would silently pad the
	// snapshot with stale files, so it is always rebuilt from empty.
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("clearing staging area: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	for _, d := range []string{DirDatabase, DirAssets, DirStack} {
		if err := os.MkdirAll(filepath.Join(staging, d), 0o700); err != nil {
			return nil, err
		}
	}

	res := &Result{Instance: in.Name}

	// 1. Databases.
	engine := &pg.Engine{Docker: docker, Service: c.Docker.PostgresService, Superuser: c.Docker.PostgresSuperuser}
	if err := engine.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres is not ready: %w", err)
	}

	// Dumps are named by ROLE, not by source database name. That is what lets a
	// restore put each dump where the target configuration says it goes, instead
	// of recreating whatever name the source host happened to use — which would
	// silently write to a live database when restoring into a scratch one.
	databases := []struct{ db, role string }{{in.Database, RoleInstance}}
	if in.KeycloakDatabase != "" {
		databases = append(databases, struct{ db, role string }{in.KeycloakDatabase, RoleKeycloak})
	}
	for _, d := range databases {
		dest := filepath.Join(staging, DirDatabase, d.role+".dump")
		logf("  dumping database %s", d.db)
		n, err := engine.Dump(ctx, d.db, dest)
		if err != nil {
			return nil, err
		}
		res.DumpBytes += n
	}

	// 2. Attachments, pulled through the S3 API as ordinary files rather than as
	//    an image of the store's data directory. That keeps a restore portable
	//    to any S3 implementation, and keeps the files readable by a human.
	client, err := s3ClientFor(c, in)
	if err != nil {
		return nil, err
	}
	logf("  mirroring bucket %s", in.Bucket)
	files, bytes, err := client.MirrorDown(ctx, in.Bucket, filepath.Join(staging, DirAssets), nil)
	if err != nil {
		return nil, fmt.Errorf("mirroring attachments: %w", err)
	}
	res.AssetFiles, res.AssetBytes = files, bytes
	logf("    %d objects, %s", files, s3.FormatBytes(bytes))

	// 3. Stack files worth carrying along.
	if in.EnvFile != "" {
		if err := copyFile(in.EnvFile, filepath.Join(staging, DirStack, ".env")); err != nil {
			return nil, fmt.Errorf("copying %s: %w", in.EnvFile, err)
		}
	}
	for _, p := range in.ExtraFiles {
		if err := copyFile(p, filepath.Join(staging, DirStack, filepath.Base(p))); err != nil {
			return nil, fmt.Errorf("copying %s: %w", p, err)
		}
	}

	// 4. Manifest, so a snapshot explains itself years later.
	if err := writeManifest(ctx, staging, c, in, res, docker); err != nil {
		return nil, err
	}

	// 5. Encrypt and ship to every destination.
	//
	// The staging directory is built once and uploaded N times, so the copies
	// are of identical bytes. A destination that fails does not stop the others:
	// with an offsite copy and a local one, losing the offsite leg tonight is
	// not a reason to also skip the local one.
	targets, err := c.Targets(in)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		cp := Copy{Destination: t.Name, Repo: t.Repo}

		runner, err := resticx.NewRunner(t, out)
		if err != nil {
			cp.Err = err
			res.Copies = append(res.Copies, cp)
			logf("  %s: \033[31mfailed\033[0m %v", t.Name, err)
			continue
		}
		created, err := runner.EnsureRepo(ctx)
		if err != nil {
			cp.Err = err
			res.Copies = append(res.Copies, cp)
			logf("  %s: \033[31mfailed\033[0m %v", t.Name, err)
			continue
		}
		if created {
			logf("  %s: initialised a new encrypted repository at %s", t.Name, t.Repo)
		}

		logf("  %s: uploading snapshot", t.Name)
		// --host is pinned to the instance name rather than the machine's
		// hostname: a restore run from a different host must still land in the
		// same snapshot group, or retention would treat it as a separate series.
		id, err := runner.Backup(ctx, staging, in.Name, []string{in.Name, "omb"})
		if err != nil {
			cp.Err = err
			res.Copies = append(res.Copies, cp)
			logf("  %s: \033[31mfailed\033[0m %v", t.Name, err)
			continue
		}
		cp.SnapshotID = id
		res.Copies = append(res.Copies, cp)

		// 6. Expire old snapshots for this destination.
		if !t.Retention.Empty() {
			if _, err := runner.Forget(ctx, t.Retention, in.Name, true); err != nil {
				// A repository reachable with an append-only key cannot prune.
				// That is a deliberate configuration, so it must not fail a
				// backup that has already been stored successfully.
				logf("  %s: \033[33m!\033[0m retention step failed (the snapshot is safely stored): %v",
					t.Name, err)
			}
		}
	}

	res.Duration = time.Since(start)
	if res.Succeeded() == 0 {
		return res, fmt.Errorf("every destination failed for instance %q", in.Name)
	}
	return res, nil
}

func s3ClientFor(c *cfg.Config, in *cfg.Instance) (*s3.Client, error) {
	ak, err := cfg.MustEnv(in.S3AccessKeyEnv)
	if err != nil {
		return nil, err
	}
	sk, err := cfg.MustEnv(in.S3SecretKeyEnv)
	if err != nil {
		return nil, err
	}
	st := c.StorageFor(in)
	return s3.New(st.Endpoint, st.Region, ak, sk, st.PathStyle), nil
}

func writeManifest(ctx context.Context, staging string, c *cfg.Config, in *cfg.Instance,
	res *Result, docker *dockerx.Client) error {

	var b strings.Builder
	fmt.Fprintf(&b, "outline-backup snapshot\n")
	fmt.Fprintf(&b, "created      : %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "tool         : %s\n", version.String())
	fmt.Fprintf(&b, "instance     : %s\n", in.Name)
	if in.Description != "" {
		fmt.Fprintf(&b, "description  : %s\n", in.Description)
	}
	fmt.Fprintf(&b, "database     : %s\n", in.Database)
	if in.KeycloakDatabase != "" {
		fmt.Fprintf(&b, "keycloak db  : %s\n", in.KeycloakDatabase)
	}
	fmt.Fprintf(&b, "bucket       : %s at %s\n", in.Bucket, c.StorageFor(in).Endpoint)
	fmt.Fprintf(&b, "attachments  : %d objects, %s\n", res.AssetFiles, s3.FormatBytes(res.AssetBytes))

	if images, err := docker.RunningContainers(ctx); err == nil && len(images) > 0 {
		fmt.Fprintf(&b, "\nservices running at backup time:\n")
		for svc, name := range images {
			fmt.Fprintf(&b, "  %-18s %s\n", svc, name)
		}
	}

	fmt.Fprintf(&b, `
layout:
  %[1]s/%[4]s.dump   this wiki's database, pg_dump -Fc
  %[1]s/%[5]s.dump   the Keycloak database, if it was included
  %[2]s/             attachment objects, keys are the paths below this directory
  %[3]s/             .env and other stack files, if they were configured

Dumps are named by role, not by their original database name: the restore
target decides what each is called when it is loaded back. The names they came
from are recorded above, for reference.

to restore this snapshot onto a fresh host:
  outline-backup restore --instance %[6]s --snapshot <id>
see docs/RESTORE.md in the repository for the full runbook.
`, DirDatabase, DirAssets, DirStack, RoleInstance, RoleKeycloak, in.Name)

	return os.WriteFile(filepath.Join(staging, ManifestTxt), []byte(b.String()), 0o600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
