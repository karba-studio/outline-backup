package run

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/karba-studio/outline-backup/internal/cfg"
)

// Agent runs backups on the configured schedule until the context is cancelled.
//
// It deliberately has no persistent state: on start it computes the next run
// from the wall clock and waits. A missed window (machine asleep, container
// restarted) is not retried immediately — the next scheduled time is used
// instead, because a burst of catch-up backups at boot is rarely what anyone
// wants and can collide with the stack still coming up.
func Agent(ctx context.Context, c *cfg.Config, out io.Writer) error {
	logf := func(format string, a ...any) {
		fmt.Fprintf(out, "%s  %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}

	if c.Schedule.Mode == "" || c.Schedule.Mode == "manual" {
		return fmt.Errorf("schedule.mode is %q, so the agent would never run anything;\n"+
			"either set a schedule with `init`, or invoke `backup` from your own scheduler",
			c.Schedule.Mode)
	}

	logf("agent started: %s", c.Schedule.Describe())
	logf("instances: %s", instanceNames(c))

	for {
		next := c.Schedule.NextRun(time.Now())
		if next.IsZero() {
			return fmt.Errorf("schedule never fires; check schedule.mode and schedule.times")
		}
		wait := time.Until(next)
		logf("next run %s (in %s)", next.Format("Mon 2006-01-02 15:04 MST"), wait.Round(time.Second))

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			logf("agent stopping")
			return nil
		case <-timer.C:
		}

		for i := range c.Instances {
			in := &c.Instances[i]
			logf("backup %s: starting", in.Name)
			res, err := Backup(ctx, c, in, out)
			if err != nil {
				// One instance failing must not stop the others, and must not
				// kill the agent: tomorrow's run should still happen.
				logf("backup %s: FAILED: %v", in.Name, err)
				continue
			}
			logf("backup %s: ok, stored in %d of %d destinations, took %s",
				in.Name, res.Succeeded(), len(res.Copies), res.Duration.Round(time.Second))
			for _, cp := range res.Copies {
				if cp.Err != nil {
					logf("  %s: FAILED: %v", cp.Destination, cp.Err)
				}
			}
		}
	}
}

func instanceNames(c *cfg.Config) string {
	out := ""
	for i, in := range c.Instances {
		if i > 0 {
			out += ", "
		}
		out += in.Name
	}
	return out
}
