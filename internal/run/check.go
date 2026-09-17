package run

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/karba-studio/outline-backup/internal/cfg"
	"github.com/karba-studio/outline-backup/internal/dockerx"
	"github.com/karba-studio/outline-backup/internal/pg"
	"github.com/karba-studio/outline-backup/internal/resticx"
	"github.com/karba-studio/outline-backup/internal/s3"
)

// CheckReport accumulates pass/fail lines.
type CheckReport struct {
	out    io.Writer
	Passed int
	Failed int
}

func (r *CheckReport) pass(what string, detail string) {
	r.Passed++
	fmt.Fprintf(r.out, "\033[32mPASS\033[0m  %-46s %s\n", what, detail)
}

func (r *CheckReport) fail(what string, err error) {
	r.Failed++
	fmt.Fprintf(r.out, "\033[31mFAIL\033[0m  %-46s %v\n", what, err)
}

func (r *CheckReport) check(what string, fn func() (string, error)) {
	detail, err := fn()
	if err != nil {
		r.fail(what, err)
		return
	}
	r.pass(what, detail)
}

// Check verifies every moving part without writing anything anywhere.
// It is what `init` runs at the end, and what you run before trusting a cron.
func Check(ctx context.Context, c *cfg.Config, out io.Writer) (*CheckReport, error) {
	rep := &CheckReport{out: out}

	docker := dockerx.New(c.Docker.ComposeProject, c.Docker.ComposeFile)
	rep.check("docker reachable", func() (string, error) {
		return "", docker.Available(ctx)
	})

	rep.check("compose project has running containers", func() (string, error) {
		ps, err := docker.RunningContainers(ctx)
		if err != nil {
			return "", err
		}
		if len(ps) == 0 {
			return "", fmt.Errorf("no running containers for project %q", c.Docker.ComposeProject)
		}
		return fmt.Sprintf("%d services", len(ps)), nil
	})

	engine := &pg.Engine{
		Docker: docker, Service: c.Docker.PostgresService, Superuser: c.Docker.PostgresSuperuser,
	}
	rep.check("postgres accepting connections", func() (string, error) {
		return c.Docker.PostgresService, engine.Ping(ctx)
	})

	for i := range c.Instances {
		in := &c.Instances[i]

		rep.check(fmt.Sprintf("[%s] database %s exists", in.Name, in.Database), func() (string, error) {
			ok, err := engine.DatabaseExists(ctx, in.Database)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", fmt.Errorf("no database named %s", in.Database)
			}
			n, err := engine.CountTables(ctx, in.Database)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%d tables", n), nil
		})

		if in.KeycloakDatabase != "" {
			rep.check(fmt.Sprintf("[%s] keycloak database %s exists", in.Name, in.KeycloakDatabase),
				func() (string, error) {
					ok, err := engine.DatabaseExists(ctx, in.KeycloakDatabase)
					if err != nil {
						return "", err
					}
					if !ok {
						return "", fmt.Errorf("no database named %s", in.KeycloakDatabase)
					}
					return "present", nil
				})
		}

		rep.check(fmt.Sprintf("[%s] bucket %s readable", in.Name, in.Bucket), func() (string, error) {
			client, err := s3ClientFor(c, in)
			if err != nil {
				return "", err
			}
			objects, err := client.ListObjects(ctx, in.Bucket, "")
			if err != nil {
				return "", err
			}
			var total int64
			for _, o := range objects {
				total += o.Size
			}
			return fmt.Sprintf("%d objects, %s", len(objects), s3.FormatBytes(total)), nil
		})

		// Cross-bucket isolation: this instance's key must NOT be able to read
		// another instance's bucket. A backup tool that can read everything is
		// one credential away from being the breach.
		for j := range c.Instances {
			other := &c.Instances[j]
			if other.Name == in.Name || other.Bucket == in.Bucket {
				continue
			}
			rep.check(fmt.Sprintf("[%s] key is denied on bucket %s", in.Name, other.Bucket),
				func() (string, error) {
					client, err := s3ClientFor(c, in)
					if err != nil {
						return "", err
					}
					if _, err := client.ListObjects(ctx, other.Bucket, ""); err != nil {
						return "access denied, as it should be", nil
					}
					return "", fmt.Errorf("this key can read %s; scope it to one bucket", other.Bucket)
				})
		}

		targets, tErr := c.Targets(in)
		if tErr != nil {
			rep.fail(fmt.Sprintf("[%s] destinations resolve", in.Name), tErr)
			continue
		}
		for _, t := range targets {
			t := t
			rep.check(fmt.Sprintf("[%s] repository %q reachable", in.Name, t.Name), func() (string, error) {
				runner, err := resticx.NewRunner(t, io.Discard)
				if err != nil {
					return "", err
				}
				created, err := runner.EnsureRepo(ctx)
				if err != nil {
					return "", err
				}
				snaps, err := runner.Snapshots(ctx, in.Name)
				if err != nil {
					return "", err
				}
				if created {
					return "newly initialised, 0 snapshots", nil
				}
				if len(snaps) == 0 {
					return "reachable, no snapshots yet", nil
				}
				newest := snaps[len(snaps)-1]
				return fmt.Sprintf("%d snapshots, newest %s (%s)",
					len(snaps), newest.ShortID, newest.Time.Format(time.RFC3339)), nil
			})
		}
	}

	rep.check("restic available", func() (string, error) {
		bin, err := resticx.Locate()
		if err != nil {
			return "", err
		}
		return bin, nil
	})

	rep.check("schedule is valid", func() (string, error) {
		next := c.Schedule.NextRun(time.Now())
		if next.IsZero() {
			return c.Schedule.Describe(), nil
		}
		return fmt.Sprintf("%s; next run %s", c.Schedule.Describe(),
			next.Format("Mon 2006-01-02 15:04 MST")), nil
	})

	fmt.Fprintf(out, "\n%d passed, %d failed\n", rep.Passed, rep.Failed)
	if rep.Failed > 0 {
		return rep, fmt.Errorf("%d check(s) failed", rep.Failed)
	}
	return rep, nil
}
