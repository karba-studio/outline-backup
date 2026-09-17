// Package dockerx runs the docker CLI. The tool talks to Postgres through the
// running container rather than over TCP, which means it never needs a local
// pg_dump, and never one whose version disagrees with the server.
package dockerx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Client wraps the docker binary for one compose project.
type Client struct {
	Binary      string // "docker"
	ComposeFile string // optional -f
	Project     string // compose project name
}

// New returns a client for the given compose project.
func New(project, composeFile string) *Client {
	return &Client{Binary: "docker", ComposeFile: composeFile, Project: project}
}

// Available reports whether the docker CLI works and the daemon answers.
func (c *Client) Available(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.Binary, "version", "--format", "{{.Server.Version}}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("docker is not usable here: %s", msg)
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("docker responded but reported no server version; is the daemon running?")
	}
	return nil
}

func (c *Client) composeArgs(args ...string) []string {
	base := []string{"compose"}
	if c.ComposeFile != "" {
		base = append(base, "-f", c.ComposeFile)
	}
	if c.Project != "" {
		base = append(base, "-p", c.Project)
	}
	return append(base, args...)
}

// Project is one compose project known to the daemon.
type Project struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles"`
}

// Projects lists compose projects, so `init` can offer the stack it finds
// instead of asking someone to remember what they called it.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	out, err := c.output(ctx, "compose", "ls", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var projects []Project
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &projects); err != nil {
		return nil, fmt.Errorf("parsing compose project list: %w", err)
	}
	return projects, nil
}

// Services lists the compose services currently defined for the project.
func (c *Client) Services(ctx context.Context) ([]string, error) {
	out, err := c.output(ctx, c.composeArgs("config", "--services")...)
	if err != nil {
		return nil, err
	}
	var services []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			services = append(services, s)
		}
	}
	return services, nil
}

// RunningContainers returns "service\tname\tstatus" rows for the project.
func (c *Client) RunningContainers(ctx context.Context) (map[string]string, error) {
	out, err := c.output(ctx, c.composeArgs("ps", "--format", "{{.Service}}\t{{.Name}}")...)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		svc, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && svc != "" {
			m[svc] = name
		}
	}
	return m, nil
}

// ContainerEnv reads the environment of a running container. init uses it to
// discover the MinIO credentials Outline is already configured with, so nobody
// has to copy access keys around by hand.
func (c *Client) ContainerEnv(ctx context.Context, service string) (map[string]string, error) {
	out, err := c.output(ctx, c.composeArgs("exec", "-T", service, "env")...)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			env[k] = v
		}
	}
	return env, nil
}

// Exec runs a command inside a service container and returns its stdout.
// stdin is optional; when non-nil it is streamed to the process.
func (c *Client) Exec(ctx context.Context, service string, stdin io.Reader,
	stdout io.Writer, argv ...string) error {

	args := c.composeArgs(append([]string{"exec", "-T", service}, argv...)...)
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	var stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s in %s: %w: %s", argv[0], service, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ExecQuiet runs a command inside a container, discarding stdout.
func (c *Client) ExecQuiet(ctx context.Context, service string, argv ...string) error {
	return c.Exec(ctx, service, nil, io.Discard, argv...)
}

// CopyFrom copies a path out of a container onto the host.
func (c *Client) CopyFrom(ctx context.Context, service, containerPath, hostPath string) error {
	_, err := c.output(ctx, c.composeArgs("cp", service+":"+containerPath, hostPath)...)
	return err
}

// CopyTo copies a host path into a container.
func (c *Client) CopyTo(ctx context.Context, service, hostPath, containerPath string) error {
	_, err := c.output(ctx, c.composeArgs("cp", hostPath, service+":"+containerPath)...)
	return err
}

func (c *Client) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
