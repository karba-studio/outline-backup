# Branching and releases

```
main ──────●───────────────────●──────────────►   production; every commit is released
            \                 /
develop ──●──●───●───●───●───●──●───●──────────►   next release
              \     /   \     /
feature/…      ●───●     ●───●                     squash-merged into develop
```

| Branch | Holds | Merged from | Merged into |
|---|---|---|---|
| `main` | what is released | `release/*`, `hotfix/*` | — |
| `develop` | what is planned | `feature/*` (squash), `release/*`, `hotfix/*` | `release/*` |
| `feature/<name>` | one change | `develop` | `develop`, squashed |
| `release/x.y.z` | stabilising a version | `develop` | `main` **and** back to `develop` |
| `hotfix/x.y.z` | urgent fix to production | `main` | `main` **and** back to `develop` |

`main` is never committed to directly. Tags live on `main`, and the release
workflow refuses a tag that is not an ancestor of `main` — so a tag accidentally
cut from `develop` cannot publish a Homebrew formula.

## A change

```sh
git switch develop && git pull
git switch -c feature/rclone-destination

# ... work, commit as often as you like; the history is squashed anyway ...

git push -u origin feature/rclone-destination
```

Open a pull request into `develop` and **squash merge** it. `develop` then has
one commit per change, which is what makes the release notes readable.

Commit subjects follow [Conventional Commits](https://www.conventionalcommits.org/),
because `.goreleaser.yaml` filters the changelog on those prefixes:

```
feat: add rclone destination kind
fix: group snapshots by tag so keepLast is not doubled
docs: explain what appears in the bucket
chore: bump CI to Go 1.24
```

`docs:`, `test:` and `chore:` are excluded from release notes.

## A release

```sh
git switch develop && git pull
git switch -c release/0.2.0
```

Nothing needs a version bump — the version comes from the tag at build time and
is stamped into the binary by the linker. So a release branch exists for
stabilising: final documentation, a last `check` against a real stack, and
nothing else merged in while it settles.

When it is ready:

```sh
# 1. into main, via a pull request
git push -u origin release/0.2.0
#    open PR release/0.2.0 -> main, merge (a real merge, not a squash)

# 2. tag on main
git switch main && git pull
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0        # this is what triggers the release workflow

# 3. back into develop, so the branches do not drift
git switch develop && git pull
git merge --no-ff main
git push
```

Step 3 is the one people skip. Skip it and `develop` no longer contains what is
in production, so the next release quietly reverts whatever was fixed on the
release branch.

## A hotfix

Same shape, starting from `main` instead of `develop`:

```sh
git switch main && git pull
git switch -c hotfix/0.2.1
# ... fix ...
# PR into main, merge, tag v0.2.1, then merge main back into develop
```

## Versions

`vMAJOR.MINOR.PATCH`, and while the config schema is still moving, stay on
`v0.x` — that tells anyone who finds the repository that breaking changes are
still expected.

Bump `cfg.SchemaVersion` when `config.json` changes shape in a way an older file
cannot satisfy. `Load` refuses a version it does not understand rather than
silently misreading a field, so this is what protects someone's existing setup.

## GitHub settings to match

These cannot be set from the repository; do them once in Settings:

- **Default branch: `develop`.** Then a pull request opened from a feature
  branch targets `develop` by default, and targeting `main` becomes the
  deliberate act it should be.
- **Branch protection on `main`**: require a pull request, require the `ci`
  check to pass, and do not allow direct pushes.
- **Branch protection on `develop`**: require the `ci` check to pass.
- **Allow squash merging** (for features) **and merge commits** (for releases).
  Squashing a release into `main` would destroy the shared history between the
  two branches and make every later merge-back conflict.

## First-time bootstrap

For a repository that has no commits yet:

```sh
git add -A
git commit -m "feat: initial implementation"
git push -u origin main

git switch -c develop
git push -u origin develop
```

Then set `develop` as the default branch and add the protection rules above.
