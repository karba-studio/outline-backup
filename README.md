# outline-backup

Back up and restore [Outline](https://www.getoutline.com/) wikis — database,
attachments and stack config — to an encrypted offsite repository, with a
separate destination per wiki — or several destinations per wiki, if you want
more than one copy.

Attachments are read over the S3 API, so the object store can be MinIO, AWS S3,
Wasabi, Cloudflare R2 or anything else that speaks it. One store commonly serves
several Outline instances, each in its own bucket; that is the default shape
here, and an instance can still point at a different endpoint when it needs to.

> **Status: v0.** The code is tested, but it has not yet been proven against a
> live production stack and no restore has been performed from a real backup.
> Treat it accordingly until that changes.

```
outline-backup init      # asks everything, writes the config
outline-backup check     # verifies every part, writes nothing
outline-backup backup    # first real run
```

`init` finds your compose stack, reads each instance's storage credentials out
of the running Outline container, and asks for the rest.

---

## Contents

- [What a backup contains](#what-a-backup-contains)
- [Encryption and what it actually protects](#encryption-and-what-it-actually-protects)
- [Install](#install)
- [Scenarios](#scenarios) — the practical part
- [Commands](#commands)
- [Configuration](#configuration)
- [Design](#design)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)

A complete, worked setup — Outline and MinIO under Docker Desktop on a Windows
machine, administered from a Mac over SSH, ending in a nightly scheduled backup
— is in [docs/WINDOWS-SERVER.md](docs/WINDOWS-SERVER.md).

---

## What a backup contains

Each instance gets a self-contained snapshot, so either one can be restored
without the other:

```
MANIFEST.txt      what this is, when it was taken, which versions were running
database/         pg_dump -Fc of the wiki's database (+ Keycloak's, if enabled)
assets/           every attachment as a real file, keys preserved as paths
stack/            .env and any other files you listed
```

Attachments are mirrored out through the S3 API as ordinary `.png`, `.jpg`,
`.pdf` files rather than tarring the store's data directory. That costs a little
more time and buys two things: you can read a backup without the object store
running, and you can restore into a different S3 implementation later.

### Destinations are named, and the mapping is yours

Destinations are declared once and referenced by name, so both directions work:

```jsonc
"destinations": {
  "b2-karba":  { "kind": "b2", "bucket": "karba-kb-backup",  ... },
  "b2-amator": { "kind": "b2", "bucket": "amator-kb-backup", ... },
  "nas":       { "kind": "local", "path": "/Volumes/backup/outline", ... }
},
"instances": [
  { "name": "private",  "backupTo": ["b2-karba", "nas"] },   // one wiki, two places
  { "name": "business", "backupTo": ["b2-amator", "nas"] }   // "nas" shared by both
]
```

Repositories are **always namespaced by instance name**, so two wikis sharing a
destination still get separate repositories:

```
b2:karba-kb-backup:private        /Volumes/backup/outline/private
b2:amator-kb-backup:business      /Volumes/backup/outline/business
```

That is not tidiness — a shared repository would interleave snapshots, and one
instance's retention policy would expire the other's history.

Two wikis sharing a destination do share that destination's **encryption
password**. If they must not be readable with one key, give them two destination
entries pointing at the same bucket with different `prefix` values and different
`passwordEnv`. The config allows both; the choice is yours to make explicitly.

Storage credentials stay per instance regardless, and `check` enforces it: it
tries to read each instance's bucket with the *other* instance's key and fails
if that succeeds.

---

## Encryption and what it actually protects

Encryption happens **on the machine running the backup, before anything leaves
it**. The destination only ever receives ciphertext — contents, file names and
directory structure included.

**What this protects against:** someone getting into the storage account or
otherwise obtaining the bucket. That is the threat model it is for, and it
covers it completely. Backblaze, an attacker with your B2 key, or anyone who
ends up with a copy of the bucket sees encrypted blobs and nothing else.

**What it does not protect against:** someone who already controls the machine
running the backups. `secrets.env` is on that machine, so they can decrypt. This
is unavoidable — the machine must be able to decrypt in order to restore — and
worth stating plainly rather than implying more than is true.

**Where the key lives:** `secrets.env`, in the variable named by
its `passwordEnv`. `init` generates 32 random bytes per destination and
prints it once.

> **Save it immediately.** Losing the password makes those backups permanently
> unreadable — by you, by the storage provider, by anyone. There is no recovery
> path, and that is exactly what makes the encryption worth having. Password
> manager, plus a paper copy somewhere physical.

Two files, deliberately separated:

| File | Contains | Commit it? |
|---|---|---|
| `config.json` | structure, and the *names* of secret variables | yes, safe |
| `secrets.env` | the actual keys and passwords, mode `0600` | **never** |

---

## Install

Nothing here needs the repository cloned.

**macOS / Linux**

```sh
brew install karba-studio/tap/outline-backup
```

**Windows**

```powershell
scoop bucket add karba-studio https://github.com/karba-studio/scoop-bucket
scoop install outline-backup
```

**Any platform, no package manager** — download the archive for your OS and
architecture from [Releases](https://github.com/karba-studio/outline-backup/releases)
and put the binary on `PATH`. One static file, no runtime to install.

**Container**

```sh
docker pull ghcr.io/karba-studio/outline-backup:latest
```

Multi-arch (amd64 and arm64), bundles `restic` and the docker CLI. Needs the
Docker socket mounted — which is effectively root on the host. See
[Scenario 3](#scenario-3-container-inside-the-same-compose-project).

---

For the binary installs, `restic` must also be on `PATH`, or `OMB_RESTIC` must
point at it:

```sh
brew install restic                  # macOS / Linux
scoop install restic                 # Windows
```

Homebrew pulls it in as a dependency automatically. The container bundles it.

---

## Scenarios

### Scenario 1: Windows server, OS scheduler (no daemon)

The usual case: Docker Desktop on a home or office server, Task Scheduler
already running other maintenance. Nothing extra stays resident.

```powershell
# once, interactively
outline-backup.exe init
outline-backup.exe check
outline-backup.exe backup
```

Set `"schedule": { "mode": "manual" }` so the tool does not also schedule
itself, then register the task:

```powershell
$action  = New-ScheduledTaskAction -Execute "outline-backup.exe" `
             -Argument "backup --config C:\server\omb\config.json"
$trigger = New-ScheduledTaskTrigger -Daily -At 3am
Register-ScheduledTask -TaskName "Outline offsite backup" -Action $action `
  -Trigger $trigger -RunLevel Highest
```

Check it afterwards with `Get-ScheduledTaskInfo 'Outline offsite backup'` —
`LastTaskResult 0` means the run succeeded.

> Docker Desktop on Windows starts when your user signs in. If nobody signs in
> after a reboot, nothing runs — including this. Automatic sign-in (e.g.
> Sysinternals Autologon) is the usual fix.

### Scenario 2: Linux host, systemd timer

```ini
# /etc/systemd/system/outline-backup.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/outline-backup backup --config /etc/outline-backup/config.json

# /etc/systemd/system/outline-backup.timer
[Timer]
OnCalendar=*-*-* 03:00:00
Persistent=true      # catches up after the machine was off

[Install]
WantedBy=timers.target
```

```sh
systemctl enable --now outline-backup.timer
systemctl list-timers outline-backup.timer
```

`schedule.mode` stays `manual` here too. Plain cron works the same way.

### Scenario 3: Container inside the same compose project

When you want everything to be a compose service.

```yaml
services:
  backup:
    image: ghcr.io/karba-studio/outline-backup:latest
    restart: unless-stopped
    command: ["agent"]
    volumes:
      - ./omb:/config
      - backup-staging:/staging
      - /var/run/docker.sock:/var/run/docker.sock
    networks: [default]

volumes:
  backup-staging:
```

`init` is interactive, so run it once with a TTY:

```sh
docker compose run --rm -it backup init
```

Here `schedule.mode` should be a real schedule (`daily` etc.), because the
container's `agent` is what does the timing.

> **Understand the trade-off.** Mounting the Docker socket lets the container
> start any container as root on the host. For a tool that only needs to run
> `pg_dump` inside one container, that is a wide grant. On a machine where you
> can simply install a binary, install the binary.

### Scenario 4: How often, and how many to keep

Frequency is `schedule`; how many to keep is `retention`. They are separate on
purpose — backing up twice a day and keeping four backups are different
decisions.

```jsonc
// twice a day
"schedule": { "mode": "daily", "times": ["03:00", "15:00"], "timezone": "Europe/Warsaw" }

// every Saturday at 08:00 UTC
"schedule": { "mode": "weekly", "dayOfWeek": "saturday", "times": ["08:00"], "timezone": "UTC" }

// first of the month
"schedule": { "mode": "monthly", "dayOfMonth": 1, "times": ["02:00"] }
```

The simplest retention rule is a plain cap:

```jsonc
"retention": { "keepLast": 4 }     // never more than four backups
```

Combined with `times: ["03:00", "15:00"]` that gives you two backups a day and a
rolling window of the last two days. Calendar rules can be added on top when a
flat cap is too blunt:

```jsonc
"retention": { "keepDaily": 7, "keepWeekly": 4, "keepMonthly": 6, "keepYearly": 2 }
```

Those are calendar rules, not counts of runs: backing up twice a day with
`keepDaily: 7` still keeps seven days, not three and a half. An all-zero
retention keeps everything forever.

`outline-backup config` prints the next run time, which is the quickest way to
confirm a change did what you meant.

#### What you will actually see in the bucket

This is worth knowing before you open Backblaze and get confused.

`keepLast: 4` means **four restorable points in time**, not four files. The
destination is a restic repository, so what is physically in the bucket is:

```
config  keys/  index/  snapshots/  data/00/ data/01/ ... data/ff/
```

— hundreds of opaque, encrypted pack files. You cannot tell which belongs to
which backup, and that is deliberate: identical content is stored once and
shared between snapshots.

The view you want is:

```sh
$ outline-backup snapshots --instance private
INSTANCE  DESTINATION  SNAPSHOT  WHEN
private   b2-karba     a1b2c3d4  2026-09-17 15:00
private   b2-karba     e5f6a7b8  2026-09-17 03:00
private   b2-karba     c9d0e1f2  2026-09-16 15:00
private   b2-karba     3a4b5c6d  2026-09-16 03:00
```

Four backups, exactly as configured. Each one restores the whole wiki
independently — they are not increments that need each other.

The payoff is size. Four separate encrypted archives of a 2 GB wiki cost 8 GB.
Four deduplicated snapshots of the same wiki, where only a few documents changed
between them, cost a little over 2 GB. That is why `keepDaily: 30` is a
reasonable thing to ask for and "keep 30 full archives" is not.

If the bucket genuinely needs to contain four discrete files you can hand to
someone, that is a different design — say so and it can be added as another
destination kind.

### Scenario 5: Inspect a backup without touching anything

Do this regularly. It proves the credentials work, the password decrypts and the
snapshot is complete, and it cannot damage a live system.

```sh
outline-backup snapshots
outline-backup restore --instance private --fetch-only --target ./peek
cat ./peek/*/MANIFEST.txt
```

### Scenario 6: Restore onto a new host

You need three things: the storage credentials, the encryption password, and
this tool.

```sh
# on the new machine, with an empty stack running
outline-backup init                 # new host's compose project, same destination
outline-backup check
outline-backup restore --instance private --destination b2-karba \
  --snapshot latest --db-owner outline_private --db-owner-password "<from .env>"
```

It creates the database, the role and the bucket if they are missing, and asks
you to type the instance name before overwriting anything. Full runbook,
including what to change when the hostname changes:
[docs/RESTORE.md](docs/RESTORE.md).

### Scenario 7: Several copies of the same wiki (3-2-1)

List more than one destination and the same snapshot is shipped to each. The
staging directory is built once and uploaded N times, so the copies are of
identical bytes.

```jsonc
"backupTo": ["b2-karba", "b2-second-provider", "nas"]
```

A destination that fails does not stop the others: losing the offsite leg
tonight is no reason to also skip the local one. `backup` reports each copy
separately and only fails when *every* destination failed.

Two Backblaze accounts are just two destination entries — different bucket,
different `keyIdEnv`/`appKeyEnv`, different `passwordEnv`.

### Scenario 8: Several wikis into one storage

Point them at the same destination name:

```jsonc
"instances": [
  { "name": "private",  "backupTo": ["nas"] },
  { "name": "business", "backupTo": ["nas"] },
  { "name": "clientx",  "backupTo": ["nas"] }
]
```

Each gets its own repository under that destination
(`/Volumes/backup/outline/private`, `.../business`, `.../clientx`), so retention
and snapshot history stay independent. They share that destination's encryption
password — see [Destinations are named](#destinations-are-named-and-the-mapping-is-yours)
if that is not acceptable.

### Scenario 9: Local disk or NAS instead of the cloud

Same tool, different destination kind — still encrypted:

```jsonc
"destinations": {
  "nas": {
    "kind": "local",
    "path": "/Volumes/backup-drive/outline",
    "passwordEnv": "OMB_DEST_NAS_PASSWORD",
    "retention": { "keepDaily": 7 }
  }
}
```

`sftp` works the same way with `"path": "user@host:/srv/backups"`. A destination
can keep a shorter history than the instance asks for, which is usually what you
want from a cheap local disk.

### Scenario 10: Protect the backups from a compromised server

An attacker who owns the backup machine can reach the destination with the key
stored there. Give that key **write but not delete** permission and they can
encrypt your live data but cannot destroy your history.

Pruning then fails — deliberately. `backup` treats a failed retention step as a
warning, not a failure, precisely so this arrangement still reports success. Run
the pruning occasionally from somewhere else with a fuller key; see
[docs/B2-SETUP.md](docs/B2-SETUP.md).

### Scenario 11: Adding a third wiki later

Copy an instance block in `config.json`, change `name`, `database`, `bucket`,
the credential variable names and `backupTo`, add the new values to
`secrets.env`, then:

```sh
outline-backup check
outline-backup backup --instance newwiki
```

It can reuse an existing destination or get its own; `check` verifies whichever
you pick and refuses two destinations that resolve to the same repository.

---

## Commands

| Command | What it does |
|---|---|
| `init` | Interactive setup; writes `config.json` and `secrets.env` |
| `check` | Verifies every part end to end. Writes nothing. Run this first |
| `backup` | Runs a backup now. `--instance <name>` for just one |
| `snapshots` | Lists what is stored |
| `restore` | Puts a snapshot back. `--fetch-only` to look without touching anything |
| `agent` | Stays running and backs up on the configured schedule |
| `config` | Prints the resolved configuration, including the next run time |
| `version` | Build version |

Every command takes `--config <path>`. Resolution order: `--config`, then
`$OMB_CONFIG`, then `./omb/config.json`, then the per-user config directory.
`secrets.env` is always read from the same directory as the config.

---

## Configuration

**[`config.example.jsonc`](config.example.jsonc) is the annotated reference** —
every field, what it does, and where the encryption key goes. Comments are
supported in the real `config.json` too, so you can keep your notes next to the
settings they explain.

The shape, in brief:

```jsonc
{
  "docker":       { /* how to reach the stack; Postgres is dumped via its container */ },
  "storage":      { /* the S3 endpoint holding attachments, shared by all instances */ },
  "destinations": { /* named places backups go; referenced by instances below */ },
  "instances": [
    { /* name, database, bucket, credentials, backupTo: [...], retention */ }
  ],
  "schedule":     { /* when `agent` runs; "manual" when the OS does the timing */ }
}
```

`outline-backup config` prints the resolved mapping, which is the fastest way to
see what will actually happen:

```
NAME      DATABASE          BUCKET            BACKS UP TO
private   outline_private   outline-private   b2-karba -> b2:karba-kb-backup:private [--keep-daily 30 ...]
                                              nas -> /Volumes/backup/outline/private [--keep-daily 7]
business  outline_business  outline-business  b2-amator -> b2:amator-kb-backup:business [...]
                                              nas -> /Volumes/backup/outline/business [--keep-daily 7]
```

Note: `init` rewrites `config.json` from scratch, so comments you added by hand
are lost if you re-run it. Edit by hand after the first run, or keep an
annotated copy in version control.

---

## Design

- **No third-party Go dependencies.** Everything is standard library, including
  the AWS Signature V4 implementation. A tool holding your backup credentials
  should contain as little code you did not choose as possible. CI fails the
  build if a dependency appears.
- **Encryption and retention are [restic](https://restic.net/).** Authenticated
  encryption, content-addressed deduplication and calendar-aware expiry are the
  three things worth never hand-rolling. This tool orchestrates it.
- **Postgres is reached through its container**, so there is never a `pg_dump`
  whose version disagrees with the server.
- **Nothing is written back until everything is downloaded.** A half-finished
  restore is worse than none.
- **The repository is an ordinary restic repository.** If this tool ever gets in
  your way, `restic restore` alone recovers everything. No lock-in.

---

## Troubleshooting

**`restic was not found on PATH`** — install it, or set `OMB_RESTIC` to its full
path. The container image already includes it.

**`docker is not usable here`** — the daemon is not running, or this user cannot
reach it. On Windows, Docker Desktop starts at sign-in.

**`environment variable OMB_… is not set`** — it is named in `config.json` but
missing from `secrets.env`, usually a typo in one of the two.

**`this key can read <other bucket>`** — the object-store key is not scoped to
one bucket. Fix it at the store; the whole per-instance separation depends on it.

**Retention step failed but the backup succeeded** — expected with an
append-only destination key. See [Scenario 10](#scenario-10-protect-the-backups-from-a-compromised-server).

**A backup you have not restored is not a backup.** `restore --fetch-only` costs
nothing. A real restore into a throwaway stack once a quarter is what actually
tells you this works.

---

## Contributing

Branching, release and versioning conventions are in
[docs/BRANCHING.md](docs/BRANCHING.md): `main` is production, `develop` is the
next release, features are squash-merged into `develop`, and releases are cut as
`release/x.y.z` and tagged on `main`.

Commit subjects follow Conventional Commits — the changelog is generated from
them.

## Licence

MIT.
