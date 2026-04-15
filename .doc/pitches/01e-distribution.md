# Pitch 01e — Distribution

*Make Beacon installable by someone who isn't the person who built it. Follow-up to pitch 00 (Docker image, `go install`), pitch 00c (`beacon mcp proxy` — requires the binary on the developer's machine), and pitch 00d (binary-hosted dashboard — the binary IS the product now).*

**Appetite:** small batch (~2 days)
**Status:** Shipped — pending PR
**Owner:** Solo founder + AI producers
**Predecessor:** `pitches/00-bootstrap.md` (Docker image at `ghcr.io/luuuc/beacon`, `go install` path), `pitches/00c-mcp-client-access.md` (stdio proxy requires binary on developer's host), `pitches/00d-dashboard.md` (dashboard embedded in binary — the binary is the complete product)
**Related:** `definition/04-deployment.md` (deployment shapes: Kamal, Docker, systemd, bare binary), `definition/08-ai-workflow.md` (step 0: "install the `beacon` binary on your machine"), `decisions/0002-maket-first-integration.md` (Maket → external users rollout — this pitch enables the "external users" step)
**Ships after:** all 01a-01d sub-pitches (this is the last sub-pitch before inviting external users)

---

## Problem

Beacon runs on Maket. Nobody else can install it.

The Docker image exists (`ghcr.io/luuuc/beacon`), `go install` works if you have Go 1.25+, and the binary can be downloaded from GitHub Releases — except there are no platform binaries in the releases. The CI builds a Docker image on tag push but doesn't produce downloadable binaries for macOS (arm64/amd64) or Linux (arm64/amd64).

A developer who wants to try Beacon today needs to either: (a) have Go installed and run `go install`, (b) use Docker, or (c) clone the repo and build from source. Option (a) excludes anyone without a Go toolchain. Option (b) works for the server but not for `beacon mcp proxy` on the developer's host machine. Option (c) is what "not ready for other people" looks like.

The `beacon mcp proxy` problem is the sharpest edge. The whole AI workflow story depends on the developer having the `beacon` binary on their laptop. Claude Code spawns it as a subprocess. If installing that binary is "clone the repo and build," the MCP setup is dead on arrival for anyone outside the project.

Beyond binaries, there's no quickstart. A new user reads the README, follows links to deployment docs and client docs and definition docs, assembles the pieces mentally, and maybe gives up. Sentry, PostHog, and Plausible all have a "run this one command" getting-started story. Beacon doesn't.

---

## Appetite

**Small batch — ~2 days.** GoReleaser is well-documented and handles cross-compilation + GitHub Release upload in a single config file. The install script is a shell script that downloads the right binary. `beacon init` is the only piece that could expand — scope it tight (generate three files, no interactive prompts).

---

## Solution

### GoReleaser for platform binaries

Add `.goreleaser.yml` to the repo. On tag push (`v*`), GitHub Actions runs GoReleaser to produce:

- `beacon-darwin-arm64` (macOS Apple Silicon)
- `beacon-darwin-amd64` (macOS Intel)
- `beacon-linux-arm64`
- `beacon-linux-amd64`
- Checksums file (`beacon_checksums.txt`)

Uploaded to the GitHub Release alongside the existing Docker image. The release notes are auto-generated from conventional commits (GoReleaser supports this natively).

The existing Docker image CI workflow stays as-is — GoReleaser handles binaries, the Dockerfile handles the container image.

### Cross-platform install script

A `install.sh` hosted in the repo (and optionally at a short URL for curl-pipe-sh):

```bash
curl -fsSL https://raw.githubusercontent.com/luuuc/beacon/main/install.sh | sh
```

The script:
1. Detects OS (`uname -s`) and architecture (`uname -m`)
2. Maps to the correct binary name (`darwin-arm64`, `linux-amd64`, etc.)
3. Fetches the latest release tag from the GitHub API (`/repos/luuuc/beacon/releases/latest`)
4. Downloads the binary to `/usr/local/bin/beacon` (or `~/.local/bin/beacon` if no write access to `/usr/local/bin`)
5. Makes it executable
6. Prints the installed version

No dependencies beyond `curl` (or `wget` as fallback) and `uname`. No package manager. Works on macOS and Linux.

### `beacon init` quickstart command

A new subcommand that generates the three files a new user needs:

```bash
beacon init --database postgres --ruby
```

Generates:
1. `docker-compose.yml` — Beacon server with the chosen database adapter
2. `.mcp.json` — Claude Code MCP configuration pointing at `localhost:4680`
3. `config/initializers/beacon.rb` (if `--ruby`) — Rails initializer with sensible defaults

Each file is printed to stdout with a header explaining what it is, or written to disk if the file doesn't already exist (never overwrites). The output includes a "next steps" message: `docker compose up -d && bundle add beacon-client && rails s`.

Flags:
- `--database` / `-d`: `postgres` (default), `mysql`, `sqlite`
- `--ruby`: include Rails initializer
- `--endpoint`: override the Beacon endpoint URL (default `http://localhost:4680`)

This is a convenience, not a framework. The generated files are starting points the user edits. Don't add interactive prompts, don't detect the user's stack, don't try to be smart.

---

## Rabbit holes

### GoReleaser configuration complexity

GoReleaser has many features (Homebrew taps, Scoop manifests, Snapcraft, Docker manifest, signing). Use the minimal config: `builds` (cross-compile), `archives` (tar.gz per platform), `release` (upload to GitHub). Don't add Homebrew, Scoop, or any package manager integration — the install script covers the install story.

### Install script security

`curl | sh` is controversial but standard for developer tools. The script should be auditable (simple, no obfuscation), hosted on GitHub (versioned, reviewable), and **must** checksum-verify the downloaded binary against the checksums file in the release. Both platforms have the required tools pre-installed (`sha256sum` on Linux, `shasum` on macOS). This is ~10 lines and non-negotiable for credibility.

### `beacon init` scope creep

The temptation is to make `beacon init` smart — detect Docker, detect Rails, detect the database, ask questions. Don't. Three flags, three files, printed output. The user knows their stack better than a detection heuristic. If someone wants a different setup, they edit the generated files.

### Binary size

The beacon binary with SQLite (via modernc.org/sqlite, pure Go) is ~30-40MB. GoReleaser can compress with UPX, but that slows startup and triggers false-positive virus scanners on macOS. Ship uncompressed. The tar.gz archive provides sufficient compression for download.

---

## No-gos

- **Homebrew tap.** Maintenance overhead for marginal benefit. The install script is cross-platform and zero-maintenance.
- **APT/YUM/RPM packages.** Too many formats to maintain for a tool in alpha.
- **Windows support.** Beacon targets Linux and macOS. Windows is not a priority for self-hosted server software.
- **Auto-update mechanism.** The install script downloads a version. The user runs it again to update. No daemon, no `beacon update` command.
- **Interactive `beacon init` wizard.** No prompts. Flags only.
- **`beacon init` for non-Ruby stacks.** The `--ruby` flag generates a Rails initializer. Node/Python/Go client init is out of scope until those clients exist (pitch 02).

---

## Acceptance Criteria

1. **Platform binaries exist on every release.** A `v*` tag push produces a GitHub Release with downloadable binaries for darwin-arm64, darwin-amd64, linux-arm64, linux-amd64, plus a checksums file.
2. **The install script works cold.** `curl -fsSL .../install.sh | sh` on a fresh macOS arm64 machine installs a working `beacon` binary, verifies the checksum, and `beacon version` prints the correct version. Same on Linux amd64.
3. **`beacon init` produces a working setup.** `beacon init --database postgres --ruby` generates three valid files. `docker compose up -d` with the generated compose file starts a Beacon instance that responds on `/api/healthz`.
4. **The MCP proxy works from an installed binary.** A developer who installed via the script can add the generated `.mcp.json` to their project and see Beacon tools in Claude Code's `/mcp` list.

---

## Scope

All cards are P0.

- [x] **GoReleaser config + CI workflow** — add `.goreleaser.yml` (builds for darwin-arm64, darwin-amd64, linux-arm64, linux-amd64; archives as tar.gz; uploads to GitHub Release with checksums). Add or extend the GitHub Actions workflow to run GoReleaser on `v*` tag push. *Done when:* a tag push produces a GitHub Release with four platform binaries and a checksums file.

- [x] **Install script** — add `install.sh` to the repo root. Detects OS/arch, fetches latest release, downloads binary and checksums file, verifies SHA256 checksum (`sha256sum` on Linux, `shasum -a 256` on macOS), installs to `/usr/local/bin/beacon` (or `~/.local/bin`), makes executable, prints version. *Done when:* `curl -fsSL .../install.sh | sh` on macOS arm64 installs a working `beacon` binary with verified checksum and `beacon version` prints the correct version.

- [x] **`beacon init` subcommand** — new subcommand in `cmd/beacon/`. Generates `docker-compose.yml`, `.mcp.json`, and optionally `config/initializers/beacon.rb`. Flags: `--database` (required: postgres/mysql/sqlite), `--ruby`, `--endpoint`. No auto-detection — the user states what they want. Prints to stdout or writes files (never overwrites). Includes "next steps" message. *Done when:* `beacon init --database postgres --ruby` produces three valid files, and `docker compose up -d` with the generated compose file starts a working Beacon instance.
