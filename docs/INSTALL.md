# Installing

Three ways. They run the same binary; pick by where the stack lives.

## Native binary (recommended on a machine you already administer)

```sh
brew install karba-studio/tap/outline-backup     # macOS / Linux
```

```powershell
scoop bucket add karba-studio https://github.com/karba-studio/scoop-bucket
scoop install outline-backup                     # Windows
```

Or download from [Releases](https://github.com/karba-studio/outline-backup/releases)
and put it on `PATH`.

`restic` must also be installed (`brew install restic`, `scoop install restic`),
or `OMB_RESTIC` must point at the binary. Homebrew installs it as a dependency.

This is the smaller blast radius: it uses the `docker` CLI you already trust,
with your own permissions.

## Container

```yaml
services:
  backup:
    image: ghcr.io/karba-studio/outline-backup:latest
    restart: unless-stopped
    command: ["agent"]
    volumes:
      - ./omb:/config
      - omb-staging:/staging
      - /var/run/docker.sock:/var/run/docker.sock
    networks: [default]

volumes:
  omb-staging:
```

Add it to the same compose project as Outline so it shares the network.

**Understand the trade-off before you do this.** Mounting the Docker socket
gives the container the ability to start any container as root on the host. For
a backup tool that only needs to run `pg_dump` inside one container, that is a
large grant. It is convenient on a Linux host where you want everything to be a
compose service; on a machine where you can just install a binary, install the
binary.

Run `init` interactively once to produce the config:

```sh
docker compose run --rm -it backup init
```

## Scheduling without the agent

`agent` keeps a process alive and runs on the schedule in the config. If you
would rather use the operating system's scheduler — which survives reboots for
free and logs failures where you already look — use `backup` as a one-shot.

**Windows Task Scheduler**

```powershell
$action  = New-ScheduledTaskAction -Execute "outline-backup.exe" `
             -Argument "backup --config C:\server\omb\config.json"
$trigger = New-ScheduledTaskTrigger -Daily -At 3am
Register-ScheduledTask -TaskName "Outline offsite backup" -Action $action `
  -Trigger $trigger -RunLevel Highest
```

**systemd**

```ini
# /etc/systemd/system/omb.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/outline-backup backup --config /etc/omb/config.json

# /etc/systemd/system/omb.timer
[Timer]
OnCalendar=*-*-* 03:00:00
Persistent=true      # catches up after the machine was off

[Install]
WantedBy=timers.target
```

**cron**

```
0 3 * * *  /usr/local/bin/outline-backup backup --config /etc/omb/config.json
```

Set `schedule.mode` to `manual` in the config when the OS is doing the timing,
so nothing is scheduled twice.

## Where configuration lives

In order of precedence:

1. `--config <path>`
2. `$OMB_CONFIG`
3. `./omb/config.json`
4. the per-user config directory
   (`~/.config/outline-backup/` on Linux,
   `~/Library/Application Support/` on macOS,
   `%AppData%` on Windows)

`secrets.env` is always read from the same directory as the config.
