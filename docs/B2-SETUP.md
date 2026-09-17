# Backblaze B2 setup

## 0. The three things a restore needs

Before any of the clicking below, know what you are actually collecting. To
rebuild a wiki on a machine that has never seen this one, you need exactly
three strings per instance:

| | What it is | Where it comes from | Where it lives afterwards |
|---|---|---|---|
| **Key ID** | B2 application key ID | you create it in step 2 | `secrets.env` on the server |
| **Application key** | its secret half, shown once | step 2 | `secrets.env` on the server |
| **Encryption password** | the key that decrypts the backups | **you invent it**, once, in `init` | `secrets.env` on the server **and somewhere off the server** |

The first two are replaceable: revoke a leaked key in the B2 console and make a
new one. The third is not. It is the password of the restic repository, and it
is what makes the contents of the bucket meaningless to whoever gets into
Backblaze. Nobody — not Backblaze, not this tool, not me — can recover a
repository whose password is gone. The snapshots simply stay ciphertext forever.

So: generate it long and random, let `init` write it into `secrets.env`, and
**store a second copy somewhere that does not depend on the server surviving** —
a password manager, or paper in a drawer. If the only copy of that password is
on the machine the backups exist to replace, you do not have backups.

```sh
# a password worth using, 32 random bytes
openssl rand -base64 32
```

In the configuration it is never written in plain: `passwordEnv` names an
environment variable, and the value sits in `secrets.env` with mode `0600`
beside the config. One password per destination, so the two wikis get two
different passwords — that is the point of two accounts, and it holds for the
encryption too.

---

One bucket per Outline instance. Separate **accounts** are worth it when the
wikis belong to different businesses: a compromised login, a leaked key or a
billing problem then reaches exactly one of them. B2 accounts are keyed to an
email address, so two accounts means two addresses.

## 1. Create the bucket

In each account: **Buckets → Create a Bucket**

| Setting | Value |
|---|---|
| Bucket name | e.g. `karba-kb-backup` (globally unique across B2) |
| Files in bucket | **Private** |
| Default encryption | Disabled or enabled — your call |
| Object Lock | Optional, see below |

Server-side encryption is not the protection that matters here: everything is
already encrypted client-side before it leaves your machine, so B2 stores
ciphertext either way.

## 2. Create a scoped application key

**Application Keys → Add a New Application Key**

| Setting | Value |
|---|---|
| Name | `omb-kb` |
| Allow access to Bucket | *the one bucket*, never "All" |
| Type of Access | Read and Write |
| Allow List All Bucket Names | not needed |

**Do not use the Master Application Key.** It cannot be scoped, it can create
and delete other keys, and rotating it breaks everything else in the account. A
scoped key can be revoked on its own the moment you suspect it.

The key is shown **once**. `init` asks for it; after that it lives in
`secrets.env` with mode `0600`.

## 3. Append-only, if you want ransomware protection

An attacker who gets into the backed-up machine can reach the backups with the
key stored there. If the key cannot delete, they can encrypt your live data but
cannot destroy your history.

Create the server's key with **Read and Write** but no delete capability, and
retention pruning will fail — deliberately. Run the pruning occasionally from
somewhere else with a second, fuller key:

```sh
export RESTIC_REPOSITORY=b2:karba-kb-backup
export RESTIC_PASSWORD=... B2_ACCOUNT_ID=... B2_ACCOUNT_KEY=...
restic forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune
```

`backup` treats a failed retention step as a warning, not a failure, precisely
so this arrangement still reports success.

B2 **Object Lock** is the stronger version of the same idea: files cannot be
deleted before a date, even with a full key. It also means you cannot prune
early, so pick a lock period you are happy to pay for.

## 4. Cost

B2 is $6.95/TB/month, with the first 10 GB free **per account** — two accounts
means 20 GB free between them. Snapshots are deduplicated, so daily backups of
mostly unchanged attachments add very little after the first one.

A wiki with a few hundred MB of attachments and a year of daily history stays
comfortably inside the free tier.

Two details that matter for backups specifically: there is **no minimum storage
duration**, so retention can prune a snapshot the day after it was written
without an early-deletion charge, and egress is free up to 3× the average
monthly stored amount. A restore pulls down roughly one snapshot's worth, so it
is free in practice; beyond that threshold it is $0.01/GB.

Prices checked 2026-09-17 against backblaze.com/cloud-storage/pricing.

## 5. Check it

```sh
outline-backup check
```

This initialises the repository if it does not exist, lists snapshots, and
verifies the credentials work — without uploading anything.
