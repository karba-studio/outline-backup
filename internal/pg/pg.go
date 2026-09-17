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
		if n, cErr := e.CountTables(ctx, db); cErr != nil || n == 0 {
			return fmt.Errorf("pg_restore into %s: %w", db, err)
		}
	}

	// --no-owner above left everything owned by the superuser. Without this the
	// database is complete and the application still cannot read it.
	if err := e.reassignToDatabaseOwner(ctx, db); err != nil {
		return fmt.Errorf("restoring object ownership in %s: %w", db, err)
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

// ensureDatabase creates db if it is missing, and in either case makes sure the
// configured owner really owns it.
//
// Applying the owner only at CREATE DATABASE time is not enough: a second
// restore into a database that already exists would leave the old owner in
// place, and since reassignToDatabaseOwner hands every object to the *database*
// owner, --db-owner would then silently do nothing. That is exactly the failure
// this whole ownership path exists to prevent, so the owner is enforced on
// every restore rather than only on the first.
func (e *Engine) ensureDatabase(ctx context.Context, db, owner string) error {
	exists, err := e.DatabaseExists(ctx, db)
	if err != nil {
		return err
	}
	if !exists {
		stmt := "CREATE DATABASE " + quoteIdent(db)
		if owner != "" {
			stmt += " OWNER " + quoteIdent(owner)
		}
		_, err = e.psql(ctx, "postgres", stmt)
		return err
	}
	if owner == "" {
		return nil
	}

	current, err := e.databaseOwner(ctx, db)
	if err != nil {
		return err
	}
	if current == owner {
		return nil
	}
	_, err = e.psql(ctx, "postgres",
		"ALTER DATABASE "+quoteIdent(db)+" OWNER TO "+quoteIdent(owner))
	if err != nil {
		return fmt.Errorf("changing the owner of database %s from %s to %s: %w",
			db, current, owner, err)
	}
	return nil
}

// databaseOwner returns the role that owns db.
func (e *Engine) databaseOwner(ctx context.Context, db string) (string, error) {
	out, err := e.psql(ctx, "postgres", fmt.Sprintf(
		"SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='%s'", escapeLiteral(db)))
	if err != nil {
		return "", err
	}
	owner := strings.TrimSpace(out)
	if owner == "" {
		return "", fmt.Errorf("cannot determine the owner of database %s", db)
	}
	return owner, nil
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

// reassignToDatabaseOwner gives every object in the public schema to whoever
// owns the database.
//
// pg_restore runs here with --no-owner, so that a dump taken on one host can be
// loaded on another where the roles have different names. The cost is that
// every object ends up owned by the role doing the restore — the superuser —
// and the application's own role can then no longer read its own tables. The
// symptom is an immediate HTTP 500 from a wiki whose data is perfectly intact,
// which is a memorably bad way to discover it.
//
// The database owner is the right target, and ensureDatabase has already made
// sure it is the configured one — for a database it just created and for one
// that was already there.
func (e *Engine) reassignToDatabaseOwner(ctx context.Context, db string) error {
	owner, err := e.databaseOwner(ctx, db)
	if err != nil {
		return err
	}
	_, err = e.psql(ctx, db, fmt.Sprintf(reassignSQL, escapeLiteral(owner)))
	return err
}

// reassignSQL takes the target role as a literal, %s, and is deliberately
// tolerant: objects belonging to an extension cannot be reassigned, and that is
// not a failure.
const reassignSQL = `
DO $omb$
DECLARE
  r          record;
  owner_role text := '%s';
  stmt       text;
BEGIN
  EXECUTE format('ALTER SCHEMA public OWNER TO %%I', owner_role);

  FOR r IN SELECT tablename AS n FROM pg_tables WHERE schemaname='public' LOOP
    EXECUTE format('ALTER TABLE public.%%I OWNER TO %%I', r.n, owner_role);
  END LOOP;

  FOR r IN SELECT sequencename AS n FROM pg_sequences WHERE schemaname='public' LOOP
    EXECUTE format('ALTER SEQUENCE public.%%I OWNER TO %%I', r.n, owner_role);
  END LOOP;

  FOR r IN SELECT viewname AS n FROM pg_views WHERE schemaname='public' LOOP
    EXECUTE format('ALTER VIEW public.%%I OWNER TO %%I', r.n, owner_role);
  END LOOP;

  FOR r IN SELECT matviewname AS n FROM pg_matviews WHERE schemaname='public' LOOP
    EXECUTE format('ALTER MATERIALIZED VIEW public.%%I OWNER TO %%I', r.n, owner_role);
  END LOOP;

  FOR r IN
    SELECT t.typname AS n FROM pg_type t
    JOIN pg_namespace ns ON ns.oid = t.typnamespace
    WHERE ns.nspname='public' AND t.typtype IN ('e','d','c')
      AND NOT EXISTS (SELECT 1 FROM pg_class c WHERE c.reltype = t.oid)
  LOOP
    stmt := format('ALTER TYPE public.%%I OWNER TO %%I', r.n, owner_role);
    BEGIN EXECUTE stmt; EXCEPTION WHEN OTHERS THEN NULL; END;
  END LOOP;

  FOR r IN
    SELECT p.oid::regprocedure AS n FROM pg_proc p
    JOIN pg_namespace ns ON ns.oid = p.pronamespace
    WHERE ns.nspname='public'
      AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=p.oid AND d.deptype='e')
  LOOP
    stmt := format('ALTER FUNCTION %%s OWNER TO %%I', r.n, owner_role);
    BEGIN EXECUTE stmt; EXCEPTION WHEN OTHERS THEN NULL; END;
  END LOOP;

  EXECUTE format('GRANT ALL ON SCHEMA public TO %%I', owner_role);
END
$omb$;`
