# Releasing

The goal is that nobody ever clones this repository to use the tool: they run a
container, or install a binary. That works once a tagged release exists.

## One-time setup

**1. Create two empty public repositories** under the same owner. goreleaser
writes into them; their names are fixed by `.goreleaser.yaml`:

| Repository | Purpose |
|---|---|
| `karba-studio/homebrew-tap` | Homebrew formula, for `brew install` |
| `karba-studio/scoop-bucket` | Scoop manifest, for Windows |

Both must be **public**, or `brew` and `scoop` cannot read them anonymously.
A README with one line is enough to initialise them.

**2. Create a token so the release can write to those two repositories.**
`GITHUB_TOKEN` is scoped to this repository only, so a separate one is needed:

- GitHub → Settings → Developer settings → Personal access tokens →
  Tokens (classic) → Generate new token, `repo` scope.
- This repository → Settings → Secrets and variables → Actions → New secret,
  named `TAP_GITHUB_TOKEN`.

Without it the release still publishes binaries and container images; the tap
and bucket updates are skipped rather than failing the release, so you can cut
`v0.1.0` before those two repositories exist and wire them up later.

**3. After the first release, make the container package public.**
GHCR packages start private even when the repository is public:

- Profile/org → Packages → `outline-backup` → Package settings →
  Change visibility → Public.

Otherwise `docker pull` asks for a login.

## Cutting a release

```sh
git tag -a v0.1.0 -m "first release"
git push origin v0.1.0
```

The `release` workflow then builds binaries for Linux, macOS and Windows on both
architectures, publishes a GitHub Release with checksums, pushes multi-arch
images to `ghcr.io`, and updates the tap and bucket.

Test the whole thing locally first, without publishing anything:

```sh
goreleaser release --snapshot --clean
ls dist/
```

## Versioning

Tags are `vMAJOR.MINOR.PATCH`. While the config schema is still moving, stay on
`v0.x` — that signals to anyone who finds the repository that breaking changes
are still likely.

Bump `cfg.SchemaVersion` when `config.json` changes shape in a way older files
cannot satisfy; `Load` refuses a version it does not understand rather than
silently misreading a field.

## What users then do

None of these need the repository:

```sh
# macOS / Linux
brew install karba-studio/tap/outline-backup

# Windows
scoop bucket add karba-studio https://github.com/karba-studio/scoop-bucket
scoop install outline-backup

# anywhere with Docker
docker pull ghcr.io/karba-studio/outline-backup:latest

# or just the binary
# https://github.com/karba-studio/outline-backup/releases
```

## winget

`winget install` would require submitting a manifest to the
`microsoft/winget-pkgs` repository by pull request, and keeping it updated on
every release. Scoop covers Windows without that, so winget is deliberately not
wired up. Add it later if it turns out to matter.
