# Prerequisites

Everything needed to build, test, and run the Optical Transport Platform.

**Short version:** if you only ever run the stack through Docker, you already have everything
— skip to [Docker-only path](#docker-only-path). The host installs below are for building and
testing natively, and for driving real camera and display hardware.

Commands target Ubuntu / Debian. Equivalents for other distributions are at the bottom.

---

## Already present on this machine

Detected during setup, no action needed:

| Tool | Required | Detected |
|---|---|---|
| Go | 1.24+ | 1.26.1 |
| Node.js | 20+ | 20.20.2 |
| npm | 10+ | 10.8.2 |
| Docker Engine | 24+ | 29.1.3 |
| Docker Compose | v2 | 2.40.3 |
| PostgreSQL client (`psql`) | 14+ | present |
| `make`, `git`, `curl`, `jq`, `python3` | — | present |

---

## Tier 1 — Core native build

Needed to run `make build` and `make test` on the host rather than in a container.

Nothing to install. Go, Node, and Docker above are sufficient. The Postgres *server* used by
the integration tests runs in a container, started automatically by the test targets, so no
local Postgres server is required.

---

## Tier 2 — Real camera capture (GoCV / OpenCV)

Only needed if you build with `-tags gocv` to capture from a physical USB, GigE, or industrial
camera. Without it, the platform uses its file-based and simulated capture sources, which need
no extra packages.

```bash
sudo apt-get update && sudo apt-get install -y libopencv-dev pkg-config
```

Verify:

```bash
pkg-config --modversion opencv4
```

Ubuntu 24.04 ships OpenCV 4.6, which satisfies GoCV. If you want a newer OpenCV than your
distribution packages, use the vendored install script instead:

```bash
make opencv-install
```

That builds OpenCV from source into `/usr/local` and takes roughly 30–45 minutes.

**For GigE Vision or industrial cameras**, also install the vendor's GenICam transport layer
plus:

```bash
sudo apt-get install -y libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev v4l-utils
```

Confirm the kernel sees a USB camera with `v4l2-ctl --list-devices`.

**Camera permissions.** Add yourself to the `video` group so the process can open
`/dev/video*` without root, then log out and back in:

```bash
sudo usermod -aG video "$USER"
```

---

## Tier 3 — GPU display output (OpenGL)

Only needed if you build with `-tags opengl` to drive a real monitor. Without it, the platform
writes frames as PNG files, which needs no extra packages.

```bash
sudo apt-get update && sudo apt-get install -y \
  libgl1-mesa-dev libglu1-mesa-dev xorg-dev \
  libx11-dev libxrandr-dev libxcursor-dev libxi-dev libxinerama-dev libxxf86vm-dev
```

For headless verification of the GPU path — how CI validates it without a monitor attached:

```bash
sudo apt-get install -y xvfb mesa-utils
```

Then:

```bash
make test-opengl-headless
```

That renders through Mesa under a virtual framebuffer, reads the framebuffer back, and decodes
it, proving the GPU render path produces valid optical frames.

---

## Tier 4 — Optional infrastructure backends

Both default to zero-install alternatives, so install these only if you want to exercise the
alternate backend natively. Both also run in Docker via the compose files, which is the
easier route.

| Backend | Default (no install) | Alternative |
|---|---|---|
| Object storage | filesystem | MinIO / any S3-compatible endpoint |
| Job broker | internal Go queue | RabbitMQ |

MinIO client, if you want to inspect buckets from the shell:

```bash
curl -sSL https://dl.min.io/client/mc/release/linux-amd64/mc -o /tmp/mc \
  && sudo install -m 0755 /tmp/mc /usr/local/bin/mc
```

---

## Tier 5 — Marketing site

Covered by Node 20 and npm, already present. Dependencies install with `npm ci` inside
`marketing-site/`.

Optional, for generating Open Graph preview images locally:

```bash
sudo apt-get install -y fonts-inter
```

Any sans-serif font works; the build falls back gracefully if Inter is absent.

---

## Docker-only path

If you would rather install nothing further, every component builds and runs in containers:

```bash
make docker-build
make docker-up
```

The images bundle OpenCV and Mesa where needed, so the camera and GPU paths build and run
inside containers even on a host without those libraries. The one thing containers cannot
provide is a physical camera or monitor — to use real hardware, pass the device through:

```bash
docker run --device /dev/video0 ...   # camera
```

---

## Copy-paste: everything at once

The full set, for a machine that will build and test every code path natively:

```bash
sudo apt-get update && sudo apt-get install -y \
  build-essential pkg-config git make curl jq \
  libopencv-dev \
  libgl1-mesa-dev libglu1-mesa-dev xorg-dev \
  libx11-dev libxrandr-dev libxcursor-dev libxi-dev libxinerama-dev libxxf86vm-dev \
  xvfb mesa-utils \
  libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev v4l-utils \
  postgresql-client
```

Then verify the whole toolchain:

```bash
make doctor
```

`make doctor` reports which tiers are satisfied and which build tags are therefore available.

---

## Other distributions

**Fedora / RHEL**

```bash
sudo dnf install -y opencv-devel mesa-libGL-devel mesa-libGLU-devel \
  libX11-devel libXrandr-devel libXcursor-devel libXi-devel libXinerama-devel \
  libXxf86vm-devel xorg-x11-server-Xvfb postgresql
```

**Arch**

```bash
sudo pacman -S --needed opencv mesa glu libx11 libxrandr libxcursor libxi \
  libxinerama libxxf86vm xorg-server-xvfb postgresql-libs
```

**macOS (Homebrew)**

```bash
brew install opencv pkg-config postgresql@16
```

OpenGL is provided by the system on macOS; the X11 packages are not needed. Note that macOS
deprecated OpenGL, so the display sink runs but emits a deprecation warning.

---

## Version floors

| Component | Minimum | Why |
|---|---|---|
| Go | 1.24 | generic type inference used in the plugin registries |
| Node.js | 20 | Next.js 15 requirement |
| Docker Engine | 24 | BuildKit features used by the multi-stage builds |
| PostgreSQL | 14 | `FOR UPDATE SKIP LOCKED` job claiming, generated columns |
| OpenCV | 4.6 | GoCV binding compatibility |
