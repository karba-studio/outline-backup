# Setting up a Windows Docker host, driven from a Mac over SSH

This is the walkthrough for the case this tool was written for: Outline and
MinIO running under Docker Desktop on a Windows machine at home, administered
from a laptop over SSH. Everything below was done on a live stack; the
commands are the ones that actually ran, not sketches.

What you have at the end: an encrypted, offsite, deduplicated backup of each
wiki, taken nightly by Windows itself, with a log you can read the morning after
and a restore path you have already tested.

Paths here use `C:\server`. Substitute your own throughout.

---

## Before you start

- SSH into the Windows machine works (`ssh Person@192.168.0.250`).
- Docker Desktop is running and the Outline stack is up.
- `restic.exe` is on the server. The tool drives restic; it does not bundle it.
  Put it beside the tool, e.g. `C:\server\tools\restic.exe`.
- You know the compose project name and the Postgres service name. `docker
  compose ls` and `docker compose ps` on the server will tell you.

## 1. Put the binary on the server

From the Mac:

```sh
scp outline-backup.exe Person@192.168.0.250:C:/server/tools/outline-backup.exe
ssh Person@192.168.0.250 "C:\server\tools\outline-backup.exe version"
```

Note the mixed slashes: `scp` wants forward slashes in the remote path, and the
Windows command line wants backslashes. Both are correct in their place.

## 2. Create the storage and the key

For Backblaze B2 this is `docs/B2-SETUP.md`: one **private** bucket, one
application key scoped **to that bucket only** (never the master key), and one
encryption password that you invent and store in a password manager *before*
you put it on the server.

Read that document's section 0 if you read nothing else. The application key is
replaceable; the encryption password is not.

## 3. Put the secrets on the server

They live in `secrets.env` beside the config — never in `config.json`, which
only ever names environment variables.

Do not paste secrets onto a command line: they end up in shell history. Let
PowerShell ask for them instead, since what you type at a `Read-Host` prompt is
not recorded.

```sh
ssh Person@192.168.0.250
powershell
```

```powershell
$id  = Read-Host "B2 keyID"
$key = Read-Host "B2 applicationKey"
$pw  = Read-Host "encryption password"

Add-Content -Path C:\server\omb\secrets.env -Encoding ASCII -Value @(
  "",
  "# kb.example.com -> Backblaze B2",
  "OMB_B2_KB_KEY_ID=$id",
  "OMB_B2_KB_APP_KEY=$key",
  "OMB_B2_KB_PASSWORD=$pw"
)
```

Check the names landed without showing the values:

```powershell
(Get-Content C:\server\omb\secrets.env) -match '^OMB_' | ForEach-Object { ($_ -split '=',2)[0] }
```

A typo in a **name** surfaces later as "environment variable is not set", which
is a confusing error to meet at 3am, so it is worth ten seconds now.

## 4. Write the config

`outline-backup init` will ask for everything interactively. Editing
`config.json` by hand is equally fine — it accepts comments, so annotate it.

The shape that matters:

```jsonc
"destinations": {
  "b2-kb": {
    "kind": "b2",
    "bucket": "karba-kb-backup",
    "keyIdEnv":   "OMB_B2_KB_KEY_ID",
    "appKeyEnv":  "OMB_B2_KB_APP_KEY",
    "passwordEnv": "OMB_B2_KB_PASSWORD"
  }
},
"instances": [
  {
    "name": "private",
    "database": "outline_private",
    "bucket": "outline-private",
    "keycloakDatabase": "keycloak",
    "backupTo": ["b2-kb"],
    "retention": { "keepDaily": 7, "keepWeekly": 4, "keepMonthly": 6 }
  }
]
```

Then read it back:

```sh
ssh Person@192.168.0.250 "C:\server\tools\outline-backup.exe config --config C:\server\omb\config.json"
```

**Always pass `--config` over SSH.** The default lookup is `omb\config.json`
*relative to the working directory*, and SSH drops you in `C:\Users\<user>`,
not in `C:\server`. Without the flag you get "no configuration at
C:\Users\...\AppData\Roaming\outline-backup\config.json", which looks like a
broken install and is not one.

Confirm the printed repository is what you expect — `b2:karba-kb-backup:private`
— before going further.

## 5. Check

```sh
ssh Person@192.168.0.250 "C:\server\tools\outline-backup.exe check --config C:\server\omb\config.json"
```

This creates the encrypted repository if it does not exist and verifies every
moving part without uploading a backup: Docker, Postgres, each database, each
bucket, the destination credentials, restic, and the schedule. It also tries
each instance's object-store key against the *other* instance's bucket and
expects to be denied — an isolation check that is easy to assume and easy to
have wrong.

Fix anything red before continuing. A green `check` is the cheapest confidence
you will get all day.

## 6. The first backup

```sh
ssh Person@192.168.0.250 "C:\server\tools\outline-backup.exe backup --config C:\server\omb\config.json --instance private"
ssh Person@192.168.0.250 "C:\server\tools\outline-backup.exe snapshots --config C:\server\omb\config.json"
```

## 7. Prove it reads back

A backup that completed without errors and a backup that can be restored are
different claims. Establish the second one now, while the original is still
sitting there and a mistake costs nothing:

```sh
ssh Person@192.168.0.250 "C:\server\tools\outline-backup.exe restore --config C:\server\omb\config.json --instance private --fetch-only --target C:\server\omb\peek"
ssh Person@192.168.0.250 "type C:\server\omb\peek\C\server\omb\staging\private\MANIFEST.txt"
```

`--fetch-only` downloads and decrypts and writes nothing to Postgres or the
object store. If the manifest is readable, the password decrypts what is in the
bucket. That is the whole claim, tested.

Then remove the decrypted copy — it is plaintext on disk:

```sh
ssh Person@192.168.0.250 "rmdir /s /q C:\server\omb\peek"
```

## 8. Give the timing to Windows

`outline-backup agent` will hold a process open and run on the schedule in the
config. On a Windows server, Task Scheduler is the better host: it survives
reboots for free and logs failures where you already look.

```powershell
$action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument `
  '/c C:\server\tools\outline-backup.exe backup --config C:\server\omb\config.json >> C:\server\omb\backup.log 2>&1'

$trigger   = New-ScheduledTaskTrigger -Daily -At 3am
$principal = New-ScheduledTaskPrincipal -UserId "$env:COMPUTERNAME\Person" `
               -LogonType Interactive -RunLevel Highest
$settings  = New-ScheduledTaskSettingsSet -StartWhenAvailable `
               -ExecutionTimeLimit (New-TimeSpan -Hours 2)

Register-ScheduledTask -TaskName "Outline offsite backup" -Action $action `
  -Trigger $trigger -Principal $principal -Settings $settings -Force
```

Three details there are load-bearing:

- **`cmd /c … >> backup.log 2>&1`.** A scheduled task's output goes nowhere by
  default. Without a log, a backup that has been failing for six weeks looks
  exactly like one that has been working.
- **`-StartWhenAvailable`** runs the job after the fact if the machine was off
  at 03:00, instead of silently skipping that night.
- **`-LogonType Interactive`, as the logged-in user.** Docker Desktop on Windows
  runs inside a user session, so its named pipe does not exist for a task
  running as `SYSTEM`. Such a task fails every night with a Docker error nobody
  reads. If the machine is not kept logged in, run Docker as a service or keep
  using `agent`.

Then set `"schedule": { "mode": "manual", "times": [] }` in the config, so the
tool schedules nothing and the two cannot both fire.

## 9. Verify the task, do not assume it

```powershell
Start-ScheduledTask -TaskName "Outline offsite backup"
Start-Sleep -Seconds 90
Get-ScheduledTaskInfo -TaskName "Outline offsite backup" |
  Select-Object LastRunTime, LastTaskResult, NextRunTime
Get-Content C:\server\omb\backup.log -Tail 20
```

`LastTaskResult` must be `0`, and `snapshots` must show one more snapshot than
before. Anything else means the nightly run will fail too — find out now.

---

## Gotchas specific to this setup

**`docker pull` over SSH fails.** Windows Credential Manager is unreachable from
an SSH session, so pulls die with `error getting credentials … A specified logon
session does not exist`. Work around it with a config directory holding a dummy
credential helper:

```jsonc
// C:\server\dockercfg\config.json
{ "auths": {}, "credHelpers": { "disabled.invalid": "none" } }
```

```powershell
docker --config C:\server\dockercfg pull <image>
```

This does **not** work for `docker compose` — CLI plugins read the normal config
directory — so pull first, then `docker compose up -d --pull never`.

**The `--config` flag.** See step 4. It is the single most common confusion
here.

**Line endings.** Shell scripts copied to a Linux container from Windows need
LF. A `.sh` with CRLF fails with an unhelpful `not found`.

**Backups and the same disk.** A destination of `kind: local` pointing at the
machine being backed up protects against a deleted document and a bad
migration, and against nothing else. It is a reasonable second copy and never a
first one.

## Adding a second destination later

Nothing in the setup above has to change. Add the destination and list it:

```jsonc
"destinations": {
  "b2-kb":     { "...": "..." },
  "b2-amator": { "kind": "b2", "bucket": "karba-amator-kb-backup",
                 "keyIdEnv": "OMB_B2_AMATOR_KEY_ID",
                 "appKeyEnv": "OMB_B2_AMATOR_APP_KEY",
                 "passwordEnv": "OMB_B2_AMATOR_PASSWORD" }
},
"instances": [
  { "name": "private",  "backupTo": ["b2-kb"],     "...": "..." },
  { "name": "business", "backupTo": ["b2-amator"], "...": "..." }
]
```

Separate accounts mean separate passwords, separate keys and separate billing,
so one compromised login reaches one wiki. An instance can also list several
destinations; the staging directory is built once and uploaded to each, and a
destination that fails does not stop the others.

Run `check` again afterwards. It is idempotent, and it initialises the new
repository for you.
