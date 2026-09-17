package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/karba-studio/outline-backup/internal/cfg"
	"github.com/karba-studio/outline-backup/internal/dockerx"
	"github.com/karba-studio/outline-backup/internal/pg"
	"github.com/karba-studio/outline-backup/internal/resticx"
	"github.com/karba-studio/outline-backup/internal/s3"
)

// RestoreOptions controls how much of a snapshot is put back.
type RestoreOptions struct {
	// Destination is the name of the destination to restore from. Optional when
	// the instance has exactly one.
	Destination string
	Snapshot    string // snapshot id, or "latest"
	// Target is where the snapshot is materialised. When empty a directory
	// under the staging area is used and removed afterwards.
	Target string
	// FetchOnly stops after downloading and decrypting: nothing is written to
	// Postgres or MinIO. This is the safe way to inspect a backup.
	FetchOnly bool
	// Database and Assets select which halves to put back.
	Database bool
	Assets   bool
	// DBOwner and DBOwnerPassword create the owning role if it is missing,
	// which is what a genuinely fresh host needs.
	DBOwner         string
	DBOwnerPassword string
}

// RestoreResult reports what was put back.
type RestoreResult struct {
	Target      string
	Restored    []string
	AssetFiles  int
	AssetBytes  int64
	TablesAfter int
}

// Restore materialises a snapshot and optionally loads it into a live stack.
//
// The order matters: everything is downloaded and verified before anything is
// written to the destination, so a half-downloaded backup cannot leave a
// half-restored wiki behind.
func Restore(ctx context.Context, c *cfg.Config, in *cfg.Instance, opt RestoreOptions,
	out io.Writer) (*RestoreResult, error) {

	logf := func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) }

	src, err := c.Target(in, opt.Destination)
	if err != nil {
		return nil, err
	}
	runner, err := resticx.NewRunner(*src, out)
	if err != nil {
		return nil, err
	}
	logf("  restoring from destination %q (%s)", src.Name, src.Repo)

	target := opt.Target
	ephemeral := false
	if target == "" {
		target = filepath.Join(c.Staging(), "restore-"+in.Name)
		ephemeral = true
	}
	if err := os.RemoveAll(target); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return nil, err
	}
	if ephemeral && !opt.FetchOnly {
		defer func() { _ = os.RemoveAll(target) }()
	}

	logf("  fetching snapshot %s", orLatest(opt.Snapshot))
	if err := runner.Restore(ctx, opt.Snapshot, target); err != nil {
		return nil, err
	}

	// restic restores the original absolute path underneath the target, so the
	// snapshot contents sit one or more directories down. Find the manifest to
	// locate the real root rather than guessing at path shapes.
	root, err := findSnapshotRoot(target)
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{Target: root}
	logf("  snapshot contents at %s", root)

	if opt.FetchOnly {
		logf("  fetch-only: nothing was written to Postgres or the object store")
		return res, nil
	}

	docker := dockerx.New(c.Docker.ComposeProject, c.Docker.ComposeFile)
	if err := docker.Available(ctx); err != nil {
		return nil, err
	}

	if opt.Database {
		engine := &pg.Engine{
			Docker: docker, Service: c.Docker.PostgresService, Superuser: c.Docker.PostgresSuperuser,
		}
		if err := engine.Ping(ctx); err != nil {
			return nil, fmt.Errorf("postgres is not ready: %w", err)
		}
		dumps, err := filepath.Glob(filepath.Join(root, DirDatabase, "*.dump"))
		if err != nil {
			return nil, err
		}
		if len(dumps) == 0 {
			return nil, fmt.Errorf("no database dumps found in %s", filepath.Join(root, DirDatabase))
		}
		for _, d := range dumps {
			db := strings.TrimSuffix(filepath.Base(d), ".dump")
			owner, password := "", ""
			// Only the instance's own database gets the configured owner; the
			// shared Keycloak dump keeps whatever the dump carries.
			if db == in.Database {
				owner, password = opt.DBOwner, opt.DBOwnerPassword
			}
			logf("  restoring database %s", db)
			if err := engine.Restore(ctx, db, d, owner, password); err != nil {
				return nil, err
			}
			res.Restored = append(res.Restored, "database:"+db)
		}
		if n, err := engine.CountTables(ctx, in.Database); err == nil {
			res.TablesAfter = n
			logf("    %s now has %d tables", in.Database, n)
		}
	}

	if opt.Assets {
		assets := filepath.Join(root, DirAssets)
		if _, err := os.Stat(assets); err != nil {
			return nil, fmt.Errorf("no assets directory in snapshot: %w", err)
		}
		client, err := s3ClientFor(c, in)
		if err != nil {
			return nil, err
		}
		exists, err := client.BucketExists(ctx, in.Bucket)
		if err != nil {
			return nil, fmt.Errorf("checking bucket %s: %w", in.Bucket, err)
		}
		if !exists {
			logf("  creating bucket %s", in.Bucket)
			if err := client.MakeBucket(ctx, in.Bucket); err != nil {
				return nil, fmt.Errorf("creating bucket %s: %w", in.Bucket, err)
			}
		}
		logf("  uploading attachments to %s", in.Bucket)
		files, bytes, err := client.MirrorUp(ctx, assets, in.Bucket, nil)
		if err != nil {
			return nil, err
		}
		res.AssetFiles, res.AssetBytes = files, bytes
		res.Restored = append(res.Restored, fmt.Sprintf("assets:%d objects", files))
		logf("    %d objects, %s", files, s3.FormatBytes(bytes))
	}

	return res, nil
}

func orLatest(s string) string {
	if s == "" {
		return "latest"
	}
	return s
}

// findSnapshotRoot locates the directory containing MANIFEST.txt.
func findSnapshotRoot(target string) (string, error) {
	var found string
	err := filepath.Walk(target, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && filepath.Base(p) == ManifestTxt {
			found = filepath.Dir(p)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil && found == "" {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("restored data in %s does not contain %s; "+
			"is this snapshot from a different tool?", target, ManifestTxt)
	}
	return found, nil
}
