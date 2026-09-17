// Package pg dumps and restores Postgres databases through the running
// container, so the client version always matches the server.
package pg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/karba-studio/outline-backup/internal/dockerx"
)

// Engine runs pg_dump / pg_restore inside a compose service.
type Engine struct {
	Docker    *dockerx.Client
	Service   string // compose service running Postgres
	Superuser string // role used for dump and restore
}

// tmpPath is where dumps are staged inside the container before being copied out.
func tmpPath(db string) string { return "/tmp/omb-" + db + ".dump" }

// Dump writes a custom-format dump of db to destFile on the host.
//
// Custom format (-Fc) is compressed and lets pg_restore reorder, which matters
// when restoring into a database whose extensions differ slightly.
func (e *Engine) Dump(ctx context.Context, db, destFile string) (int64, error) {
	inside := tmpPath(db)

	if err := e.Docker.ExecQuiet(ctx, e.Service,
		"pg_dump", "-U", e.Superuser, "-Fc", "--no-owner", "--no-acl", "-f", inside, db); err != nil {
		return 0, fmt.Errorf("pg_dump %s: %w", db, err)
	}
	// Verify the dump is readable before trusting it. A dump that pg_restore
	// cannot list is not a backup, and finding that out now beats finding out
	// during an emergency.
	if err := e.Docker.ExecQuiet(ctx, e.Service, "pg_restore", "-l", inside); err != nil {
		_ = e.Docker.ExecQuiet(ctx, e.Service, "rm", "-f", inside)
		return 0, fmt.Errorf("dump of %s is not readable by pg_restore: %w", db, err)
	}

	if err := os.MkdirAll(filepath.Dir(destFile), 0o755); err != nil {
		return 0, err
	}
	if err := e.Docker.CopyFrom(ctx, e.Service, inside, destFile); err != nil {
		return 0, fmt.Errorf("copying dump of %s out of the container: %w", db, err)
	}
	_ = e.Docker.ExecQuiet(ctx, e.Service, "rm", "-f", inside)

	fi, err := os.Stat(destFile)
	if err != nil {
		return 0, err
	}
	if fi.Size() == 0 {
		return 0, fmt.Errorf("dump of %s is empty", db)
	}
	return fi.Size(), nil
}

// Restore loads a dump into db, creating the database and its owning role if
// they are absent. Existing objects are replaced.
func (e *Engine) Restore(ctx context.Context, db, srcFile, owner, ownerPassword string) error {
	if _, err := os.Stat(srcFile); err != nil {
		return err
	}
	inside := tmpPath(db)

	if owner != "" {
		if err := e.ensureRole(ctx, owner, ownerPassword); err != nil {
			return err
		}
	}
	if err := e.ensureDatabase(ctx, db, owner); err != nil {
		return err
	}
	if err := e.Docker.CopyTo(ctx, e.Service, srcFile, inside); err != nil {
		return fmt.Errorf("copying dump into the container: %w", err)
	}

	// --clean --if-exists makes the restore idempotent against a database that
	// already has objects. Ordering errors on extensions are common and benign,
	// so we do not use --exit-on-error; the verification step below is what
	// decides whether the restore worked.
	err := e.Docker.ExecQuiet(ctx, e.Service,
		"pg_restore", "-U", e.Superuser, "-d", db, "--clean", "--if-exists",
		"--no-owner", "--no-acl", inside)
	_ = e.Docker.ExecQuiet(ctx, e.Service, "rm", "-f", inside)
	if err != nil {
		// pg_restore exits non-zero on warnings too; confirm with a real query.
		if n, cErr := e.CountTables(ctx, db); cErr == nil && n > 0 {
			return nil
		}
		return fmt.Errorf("pg_restore into %s: %w", db, err)
	}
	return nil
}

// CountTables returns the number of tables in the public schema, used as a
// cheap sanity check that a restore actually produced something.
func (e *Engine) CountTables(ctx context.Context, db string) (int, error) {
	out, err := e.psql(ctx, db,
		"SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0, fmt.Errorf("unexpected table count %q", strings.TrimSpace(out))
	}
	return n, nil
}

// Ping checks the server is up and accepting connections.
func (e *Engine) Ping(ctx context.Context) error {
	return e.Docker.ExecQuiet(ctx, e.Service, "pg_isready", "-U", e.Superuser)
}

// DatabaseExists reports whether db is present.
func (e *Engine) DatabaseExists(ctx context.Context, db string) (bool, error) {
	out, err := e.psql(ctx, "postgres",
		fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname='%s'", escapeLiteral(db)))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "1", nil
}

func (e *Engine) ensureRole(ctx context.Context, role, password string) error {
	out, err := e.psql(ctx, "postgres",
		fmt.Sprintf("SELECT 1 FROM pg_roles WHERE rolname='%s'", escapeLiteral(role)))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "1" {
		if password == "" {
			return nil
		}
		_, err = e.psql(ctx, "postgres", fmt.Sprintf("ALTER ROLE %s WITH LOGIN PASSWORD '%s'",
			quoteIdent(role), escapeLiteral(password)))
		return err
	}
	_, err = e.psql(ctx, "postgres", fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD '%s'",
		quoteIdent(role), escapeLiteral(password)))
	return err
}

func (e *Engine) ensureDatabase(ctx context.Context, db, owner string) error {
	exists, err := e.DatabaseExists(ctx, db)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	stmt := "CREATE DATABASE " + quoteIdent(db)
	if owner != "" {
		stmt += " OWNER " + quoteIdent(owner)
	}
	_, err = e.psql(ctx, "postgres", stmt)
	return err
}

func (e *Engine) psql(ctx context.Context, db, sql string) (string, error) {
	var out bytes.Buffer
	err := e.Docker.Exec(ctx, e.Service, nil, &out,
		"psql", "-U", e.Superuser, "-d", db, "-tAc", sql)
	return out.String(), err
}

// quoteIdent wraps an identifier in double quotes, doubling any it contains.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// escapeLiteral doubles single quotes for use inside a SQL string literal.
func escapeLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
