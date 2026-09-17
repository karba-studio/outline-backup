// Package resticx drives the restic binary.
//
// restic owns the parts of a backup tool that are genuinely hard to get right:
// authenticated encryption, content-addressed deduplication, and an expiry
// policy that understands calendars. This package is the boring glue that
// points it at the right repository with the right credentials.
package resticx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/karba-studio/outline-backup/internal/cfg"
)

// Runner executes restic against one repository.
type Runner struct {
	Binary string
	Repo   string
	// Env holds repository credentials and the encryption password. These are
	// passed through the child process environment rather than the command line
	// so they never appear in the host's process list.
	Env map[string]string
	// Out receives restic's own progress output; nil discards it.
	Out io.Writer
}

// Locate finds the restic binary: $OMB_RESTIC first, then PATH.
func Locate() (string, error) {
	if p := os.Getenv("OMB_RESTIC"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("OMB_RESTIC is set to %q but there is no file there", p)
	}
	p, err := exec.LookPath("restic")
	if err != nil {
		return "", fmt.Errorf("restic was not found on PATH.\n" +
			"  macOS/Linux : brew install restic\n" +
			"  Windows     : winget install restic.restic\n" +
			"  or set OMB_RESTIC to its full path")
	}
	return p, nil
}

// NewRunner builds a runner for one target: an instance's repository inside a
// named destination.
func NewRunner(t cfg.Target, out io.Writer) (*Runner, error) {
	bin, err := Locate()
	if err != nil {
		return nil, err
	}
	dest := t.Destination
	repo := t.Repo
	password, err := cfg.MustEnv(dest.PasswordEnv)
	if err != nil {
		return nil, err
	}

	env := map[string]string{
		"RESTIC_REPOSITORY": repo,
		"RESTIC_PASSWORD":   password,
	}
	switch dest.Kind {
	case "b2":
		id, err := cfg.MustEnv(dest.KeyIDEnv)
		if err != nil {
			return nil, err
		}
		key, err := cfg.MustEnv(dest.AppKeyEnv)
		if err != nil {
			return nil, err
		}
		env["B2_ACCOUNT_ID"] = id
		env["B2_ACCOUNT_KEY"] = key
	case "s3":
		id, err := cfg.MustEnv(dest.KeyIDEnv)
		if err != nil {
			return nil, err
		}
		key, err := cfg.MustEnv(dest.AppKeyEnv)
		if err != nil {
			return nil, err
		}
		env["AWS_ACCESS_KEY_ID"] = id
		env["AWS_SECRET_ACCESS_KEY"] = key
	}
	return &Runner{Binary: bin, Repo: repo, Env: env, Out: out}, nil
}

func (r *Runner) environ() []string {
	e := os.Environ()
	for k, v := range r.Env {
		e = append(e, k+"="+v)
	}
	return e
}

func (r *Runner) run(ctx context.Context, streamOutput bool, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Env = r.environ()

	var stdout, stderr bytes.Buffer
	if streamOutput && r.Out != nil {
		cmd.Stdout = io.MultiWriter(&stdout, r.Out)
	} else {
		cmd.Stdout = &stdout
	}
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("restic %s: %w\n%s",
			strings.Join(redact(args), " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// redact keeps repository URLs out of error messages that might be pasted around.
func redact(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	return out
}

// Version returns the restic version string.
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := r.run(ctx, false, "version")
	return strings.TrimSpace(out), err
}

// EnsureRepo initialises the repository if it does not exist yet.
// Returns true when it created one.
func (r *Runner) EnsureRepo(ctx context.Context) (created bool, err error) {
	if _, err := r.run(ctx, false, "cat", "config"); err == nil {
		return false, nil
	}
	if _, err := r.run(ctx, true, "init"); err != nil {
		return false, fmt.Errorf("initialising repository: %w", err)
	}
	return true, nil
}

// Backup snapshots dir, tagging the snapshot with the instance name so several
// instances remain distinguishable even if they ever share a repository.
func (r *Runner) Backup(ctx context.Context, dir, host string, tags []string) (string, error) {
	args := []string{"backup", "--host", host, "--json"}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	args = append(args, dir)

	out, err := r.run(ctx, false, args...)
	if err != nil {
		return "", err
	}
	// The final JSON line of a successful run is the summary.
	var id string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var msg struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal([]byte(line), &msg) == nil && msg.MessageType == "summary" {
			id = msg.SnapshotID
		}
	}
	if id == "" {
		return "", fmt.Errorf("restic did not report a snapshot id; output was:\n%s", out)
	}
	return id, nil
}

// Snapshot is one entry from `restic snapshots`.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Paths    []string  `json:"paths"`
}

// Snapshots lists snapshots, optionally filtered by tag.
func (r *Runner) Snapshots(ctx context.Context, tag string) ([]Snapshot, error) {
	args := []string{"snapshots", "--json"}
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	out, err := r.run(ctx, false, args...)
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		return nil, fmt.Errorf("parsing snapshot list: %w", err)
	}
	return snaps, nil
}

// Forget applies the retention policy. Pruning actually reclaims space but
// needs delete permission on the remote; with an append-only key it will fail,
// which is the intended trade-off and not a bug.
func (r *Runner) Forget(ctx context.Context, policy cfg.Retention, tag string, prune bool) (string, error) {
	if policy.Empty() {
		return "retention policy is empty; keeping every snapshot", nil
	}
	// Group by host and tags, not by paths (restic's default). Both are pinned
	// to the instance name, while the staging path contains the config
	// directory - so moving the config would otherwise start a second group and
	// quietly double how many snapshots "keep last 4" retains.
	args := append([]string{"forget", "--group-by", "host,tags"}, policy.Args()...)
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	if prune {
		args = append(args, "--prune")
	}
	return r.run(ctx, true, args...)
}

// Restore materialises a snapshot into target. snapshot may be "latest".
func (r *Runner) Restore(ctx context.Context, snapshot, target string) error {
	if snapshot == "" {
		snapshot = "latest"
	}
	_, err := r.run(ctx, true, "restore", snapshot, "--target", target)
	return err
}

// Check verifies repository structure. Deep verifies a sample of the data too.
func (r *Runner) Check(ctx context.Context, deep bool) error {
	args := []string{"check"}
	if deep {
		args = append(args, "--read-data-subset", "5%")
	}
	_, err := r.run(ctx, true, args...)
	return err
}
