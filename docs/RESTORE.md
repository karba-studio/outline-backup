# Restoring onto a new host

What you need in your hands:

1. The **storage credentials** for that instance's repository (for Backblaze:
   the application key ID and the application key).
2. The **encryption password** for that repository.
3. This tool, and `restic`.

Nothing else. The snapshot carries the database, the attachments, the stack
`.env` and a manifest describing what it came from.

---

## 0. Look before you touch anything

Do this first, always. It proves the credentials work, the password decrypts,
and the snapshot is complete — before any of it is pointed at a live system.

```sh
outline-backup restore --instance private --fetch-only --target ./peek
ls ./peek
cat ./peek/*/MANIFEST.txt
```

`--fetch-only` never writes to Postgres or the object store.

## 1. Bring up an empty stack

On the new host, start Outline, Postgres, Keycloak and the object store as
usual. They can
be empty; the restore creates the database, the role and the bucket if they are
missing.

If you kept `.env` in the backup, take it from `stack/.env` in the peeked
snapshot and use it. Outline encrypts some stored tokens with `SECRET_KEY`, so
reusing the original `.env` means integrations keep working rather than needing
to re-authenticate.

## 2. Point the tool at the new stack

```sh
outline-backup init
```

Answer with the *new* host's compose project and storage endpoint, and the *same*
destination, key and password as the old one. Then:

```sh
outline-backup check
```

## 3. Restore

```sh
outline-backup restore --instance private --snapshot latest \
  --db-owner outline_private --db-owner-password "<from .env>"
```

`--db-owner` does three things, in this order: it creates the owning role if the
new Postgres has never seen it, it makes that role the owner of the target
database (whether the restore created that database or it was already there),
and then every table, sequence, view and function is handed to it once the dump
is loaded.

That last step is not cosmetic. Dumps are loaded with `--no-owner`, so that a
snapshot from one host can be restored on another where the roles are named
differently — which leaves everything owned by the superuser doing the restore.
A wiki whose data is completely intact but whose own role cannot read its tables
answers every request with **HTTP 500**, and it is not an obvious thing to
diagnose. Passing `--db-owner` is what prevents that.

Omit it only when the target database is already owned by the right role; the
ownership pass then hands objects to that existing owner.

It will ask you to type the instance name before overwriting anything. Use
`--yes` in a script, not at a keyboard.

Useful flags:

- `--snapshot <id>` — a specific snapshot rather than the newest. `snapshots`
  lists them.
- `--skip-assets` / `--skip-database` — restore one half.
- `--target <dir>` — keep the decrypted copy instead of using a temporary one.

## 4. Point traffic at it

The restore puts data back; it does not touch DNS or your tunnel. Update
whatever routes `kb.example.com` to the new host, and if the **hostname
changed**, also update:

- `URL` in `.env` for that instance,
- the redirect URIs in the Keycloak client,
- `AWS_S3_UPLOAD_BUCKET_URL`, if the object store moved too.

Outline signs attachment URLs for the browser, so an S3 endpoint the browser
cannot reach means uploads that fail at the moment someone drags in a file.

## 5. Verify like you mean it

```sh
outline-backup check
```

Then open the wiki and confirm three things by hand: a document loads, an image
inside a document renders, and login works. The first proves Postgres, the
second proves the object store and the signed-URL path, the third proves Keycloak.

---

## Restoring somewhere else entirely

The layout is deliberately plain, so you are never locked into this tool:

```
database/instance.dump           pg_restore -d <your database> --clean --if-exists
database/keycloak.dump           the Keycloak database, if it was included
assets/uploads/<team>/<id>/<f>   keys are the paths below assets/
stack/.env                       the original environment
```

Dumps are named by **role**, not by the database name they came from. That is
deliberate: the restore target decides where each one goes, so restoring a
production snapshot into a scratch database cannot reach the production one by
accident. `MANIFEST.txt` records the original names.

With `--fetch-only` and those two facts you can restore by hand into any
Postgres and any S3-compatible store.

## If restic is all you have

The repository is an ordinary restic repository. Without this tool:

```sh
export RESTIC_REPOSITORY=b2:karba-kb-backup
export RESTIC_PASSWORD=...
export B2_ACCOUNT_ID=... B2_ACCOUNT_KEY=...
restic snapshots
restic restore latest --target ./out
```
