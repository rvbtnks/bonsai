# bonsai-monitor

One ASCII bonsai tree for your whole LAN. A server draws it in your terminal;
every other machine runs a tiny client that reports four numbers. A healthy
network is a full, trim, green tree. A sick one is sparse, drooping, with red
patches that need pruning — readable at a glance from an SSH session inside
tmux.

```
                                                 @#   *ooo@
                                          o *@#\ //|\\*#@@**o
                                         @*o#*@#\\ |oo#*@o*##/   *@*#%o@@
                                        *o#*@o@@#\\| %@#oo#@/  #*@o#@@@o@#
                                       *o*##o*@%|  | *@*@**#- %*#**o##%*o@#
                                       %*%##/@\  | |@o@@o#\*#-@o***o@*#*##o
                                       %**%/%% \\|  |*o#o**o   @o@@%##@##@o
                                          \\     |  //@o%@#        \/**@@o
                                                  \ | /              /   \
                                                  \ |//
                                                   \|/
                                                   \/
                                                    /
                                                    |
                                                    ||
                                                    |
                                                    ||
                                                    ||
                                         +--------,,|-,-,------+
                                          \                   /
                                           `-----------------'
 network 81% thriving  [#####################-----] hosts 8  7 ok  1 warn        up 15s
 q quit    l hosts    ? help
```

Two commands are the whole interface:

```console
$ ./bonsai-server                 # the tree appears, listening on UDP+TCP 1717
$ ./bonsai-client 192.168.1.1     # one line, prompt back, keeps reporting in the background
bonsai-client started (pid 4242) -> 192.168.1.1:1717
```

No config file, no environment variables, no discovery, no agents to configure.
Zero-config is the point: every option has a default and the common case needs
no flags at all.

---

## ⚠️ LAN only — read this first

**There is no authentication and no encryption.** The protocol is a QOTD
variant: anyone who can reach port 1717 can read a quote and can claim to be
any hostname with any metrics. The server treats every byte it receives as
untrusted input (hard 512-byte cap, strict hostname charset, numeric clamping,
`strconv` only, nothing is ever executed), so it will not fall over — but it
will happily believe a liar.

Run it on a network you trust. If that network is bigger than a home or a lab,
firewall the port:

```console
# only the management subnet may talk to the tree
sudo ufw allow from 192.168.1.0/24 to any port 1717 proto tcp
sudo ufw allow from 192.168.1.0/24 to any port 1717 proto udp
sudo ufw deny 1717
```

Never expose 1717 to the internet.

---

## Contents

- [Quick start](#quick-start)
- [Reading the tree](#reading-the-tree)
- [Building](#building)
- [The client](#the-client)
- [Making it survive a reboot](#making-it-survive-a-reboot-systemd-and-cron)
- [Watching over SSH and tmux](#watching-over-ssh-and-tmux)
- [Headless mode](#headless-mode-no-terminal-no-problem)
- [The simulator](#the-simulator)
- [Protocol](#protocol)
- [Health scoring](#health-scoring)
- [Tuning thresholds](#tuning-thresholds)
- [Manual tests](#manual-tests)
- [Troubleshooting](#troubleshooting)
- [Repository layout](#repository-layout)

---

## Quick start

On the machine that will show the tree (a spare Pi, a home server, your
laptop in a tmux pane):

```console
$ ./bonsai-server
```

On every machine you want in the picture:

```console
$ ./bonsai-client 192.168.1.1
bonsai-client started (pid 4242) -> 192.168.1.1:1717
```

That is it. The command returns the prompt immediately, the client detaches
into its own session, and the machine's leaf cluster appears in the tree within
one report interval (10 s by default). Its name is read from `os.Hostname()`.

To check on it later, from the same machine:

```console
$ ./bonsai-client -status
running (pid 4242) -> 192.168.1.1:1717
$ ./bonsai-client -stop
stopped
```

Point a machine at a different server by simply running the command again with
the new address — the newest invocation wins and the old one is stopped:

```console
$ ./bonsai-client 10.0.0.9:1717
bonsai-client started (pid 5150) -> 10.0.0.9:1717
```

No other arguments? You get a one-line usage message and exit code 2.

---

## Reading the tree

**One tree, one organism.** The tree is not a per-host chart; its shape, colour
and posture are driven by a single number — network vitality.

| Network vitality | What you see |
| --- | --- |
| above 0.8 | full balanced canopy, green, upright trunk, slow steady growth |
| 0.5 – 0.8 | growth pauses, canopy thins, some yellowing, a slight lean |
| below 0.5 | sparse and drooping, heavy red and brown, dead twigs, fallen leaves around the pot |

**Each machine owns one leaf cluster.** Its position is derived from a hash of
the hostname, so it is stable across restarts — `nas01` is always that branch,
third from the left. The cluster's colour is that host's own vitality: green
when healthy, yellow/orange as it degrades, red when critical, brown and bare
when it has gone quiet. The number of hosts sets the canopy size; every new
host adds a shoot.

**Labels stay out of the picture.** The main view shows no hostnames at all.
Names and the offending metric appear in exactly two places: next to a *red*
cluster (that is your pruning target), and in the `l` overlay.

```
                                           *X#  |   \o*#*@o#|       *%o
                                        o%#XX## |sim-07 cpu 93%  ###**@@*
                                        o*%/XX-||    @#o%\\o/- XX*#Xo#sim-05 cpu 91%
                                        **o/*   \     #**#*o    #*\**X#X## sim-06 temp 89C
                                          \\    \     |/o#-\      \\****\   sim-08 cpu 92%
 network 29% stressed  [########------------------] hosts 8  3 ok  1 warn  4 crit   up 45s
 q quit    l hosts    ? help                                    4 need pruning - press l
```

**Growth is global and honest.** While vitality stays high the whole tree
gains height, branching and leaf density, up to a cap that fits your terminal.
Growth slows and stops as vitality falls; sustained low vitality causes
dieback, so a network that has been sick for a while looks sick. Recovery
regrows it. Growth persists only in RAM — restart the server and you start
from a seedling again.

**The status line** carries the numbers: network vitality as a percentage, a
word (`thriving` / `ok` / `stressed` / `ailing`), a bar, host counts
(`ok` / `warn` / `crit` / stale) and server uptime.

**Keys:**

| Key | Action |
| --- | --- |
| `q` | quit |
| `l` | toggle the host overlay: every host, its metrics, vitality and worst metric |
| `?` | toggle help |
| `^L` | force a redraw |

With no clients at all you get a **seedling** and a neutral 50 %:

```
                                                   #@#
                                                  % # @
                                                   *|@
                                                    |
                                                    |
                                                +---|---+
                                                 \     /
                                                  `---'
 network 50% ok        [#############-------------] hosts 0  0 ok               up 0s
                               listening on :1717 ... waiting for clients
```

---

## Building

Requirements: **Go 1.21 or newer** and GNU make. The server has exactly one
direct third-party dependency (`github.com/gdamore/tcell/v2`, plus the indirect
packages tcell itself needs); the client is standard library only, so it builds
anywhere Go does.

```console
$ make            # ./bonsai-server and ./bonsai-client for this machine
$ make test       # go test ./...
$ make vet        # go vet ./...
$ make fmt        # gofmt -w
$ make dist       # every release binary, cross-compiled into dist/
$ make install    # copy both into ~/.local/bin
$ make simulate   # build, then drive the server with 8 fake clients
$ make clean
```

Everything is built with `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`, so the
binaries are **fully static and stripped**: no libc, no runtime dependencies.
Copy one to a machine with no Go installed and it runs.

`make dist` produces eight files:

| File | For |
| --- | --- |
| `bonsai-server-linux-amd64`, `bonsai-client-linux-amd64` | x86-64 PCs, most servers |
| `bonsai-server-linux-arm64`, `bonsai-client-linux-arm64` | Pi 3/4/5 (64-bit OS), modern ARM |
| `bonsai-server-linux-armv7`, `bonsai-client-linux-armv7` | 32-bit ARMv7 (Pi 2, older 32-bit OS) |
| `bonsai-server-linux-armv6`, `bonsai-client-linux-armv6` | old Pis: Zero, Zero W, first-gen B+/A+ |

Pick the client for the machine it runs on — armv6 and armv7 are *not*
interchangeable (an armv7 binary will not start on a Pi Zero).

The version is stamped at build time from `git describe`, falling back to
`1.0.0`; `./bonsai-client -version` prints it.

---

## The client

```console
$ ./bonsai-client HOST[:PORT]
```

`HOST` may be an IP or a hostname; `PORT` defaults to 1717. **The address is
required** — there is no auto-discovery, no broadcast, no guessing.

### What it does when you run it

1. Validates the arguments (syntax only — the server does *not* have to be
   reachable; a dead server just means quiet retries).
2. Re-launches its own binary with a hidden `-child` flag in a new session
   (`Setsid`), ignoring `SIGHUP`, with stdin/stdout/stderr on `/dev/null`. Go
   cannot safely `fork()`, so re-exec is the portable way to detach.
3. Writes the child's PID to a pidfile, prints one line, exits 0. Your prompt
   comes back in a few milliseconds.

Because it has its own session and ignores `SIGHUP`, **closing the SSH window
that started it does not stop it**.

### Flags

| Flag | Behavior |
| --- | --- |
| `-stop` | SIGTERM the running instance, remove the pidfile, print `stopped` or `not running` |
| `-status` | Print `running (pid N) -> HOST:PORT` or `not running`. Exit 0 if running, 1 if not |
| `-f` | Run in the foreground (for systemd or debugging) |
| `-v` | Verbose: print every send and reply. Implies `-f` |
| `-port N` | Override the port |
| `-interval SECONDS` | Override the 10 s interval |
| `-tcp` | Use TCP instead of UDP |
| `-name NAME` | Override the reported hostname |
| `-print-unit` | Print a ready-to-use systemd unit and exit |
| `-version` | Print the version and exit |

### Defaults

| Thing | Default |
| --- | --- |
| transport | UDP |
| interval | 10 s |
| port | 1717 |
| hostname | `os.Hostname()`, sanitised to `[A-Za-z0-9._-]`, truncated to 32 chars |
| pidfile | `$XDG_RUNTIME_DIR/bonsai-client.pid`, else `/tmp/bonsai-client-<uid>.pid` |
| log | `~/.bonsai-client.log` |

The pidfile is never trusted blindly: a PID is only accepted if the process is
alive (signal 0), not a zombie, **and** `/proc/<pid>/cmdline` really is
`bonsai-client`. A stale pidfile or a recycled PID can therefore never block a
start or make `-stop` kill a stranger.

### Metrics

| Metric | Source | Notes |
| --- | --- | --- |
| `cpu` | delta of the aggregate `cpu` line in `/proc/stat` | the first tick sends no `cpu` — there is no delta yet |
| `temp` | max over valid `/sys/class/thermal/thermal_zone*/temp` | millidegrees → °C; omitted if no zone exists (many VMs) |
| `load` | first field of `/proc/loadavg` ÷ `runtime.NumCPU()` | per-core, as the scoring expects |
| `up` | first field of `/proc/uptime` | seconds |

Any metric that cannot be read is simply omitted — the client never fails
because one file is missing.

### Logging

The background client is silent. `~/.bonsai-client.log` records start, stop and
errors only (never a successful send), and truncates itself at about 1 MB:

```
2026-10-31 13:42:44 started (pid 13997) -> 127.0.0.1:1999 name=nas01 interval=10s tcp=false
2026-10-31 13:42:46 send to 127.0.0.1:1999 failed: read udp ...: connection refused
2026-10-31 13:43:02 stopped (pid 13997)
```

If the server is down the client keeps running quietly, retries every interval,
and re-resolves the server address after a failure so a DNS or IP change heals
on its own. When the server comes back, the host reappears in the tree.

### Footprint

About **3 MB RSS** (measured median 3.1 MB, peak 3.4 MB) and negligible CPU: it
wakes once per interval, reads four small files, sends one datagram under
512 bytes, and goes back to sleep. Three deliberate choices keep it there:

- `GOMAXPROCS(1)` — one datagram every ten seconds does not need a scheduler
  slot per core, and each one costs a thread stack and its own caches.
- A tighter GC target (`SetGCPercent(40)`) plus one explicit
  `FreeOSMemory()` after startup, so the arena from flag parsing and the first
  `/proc` read goes back to the kernel instead of waiting for the scavenger.
- On kernels whose transparent huge pages are set to `always` (the RHEL-family
  default, and common in containers), the client re-execs itself once with
  `GODEBUG=disablethp=1` — same PID, same descriptors, so the pidfile and
  systemd's notion of the main process stay correct. Go 1.21 stopped working
  around that kernel default ([go#64332](https://go.dev/issue/64332)), so
  without this the heap is rounded up to 2 MB pages it will never touch. It is
  worth about 2 MB, and it is skipped entirely when THP is `madvise` or `never`,
  or when you have already set `disablethp` yourself.

---

## Making it survive a reboot: systemd and cron

`Setsid` plus ignoring `SIGHUP` survives logout, but not a reboot. For
persistence use your init system.

### systemd (one line)

```console
$ ./bonsai-client -print-unit 192.168.1.1 | sudo tee /etc/systemd/system/bonsai-client.service
$ sudo systemctl daemon-reload
$ sudo systemctl enable --now bonsai-client.service
```

`-print-unit` emits a unit with the correct **absolute** path to the binary you
ran and your server argument baked in. It uses `-f`, because systemd does its
own backgrounding:

```ini
[Unit]
Description=bonsai-monitor client for 192.168.1.1:1717
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/bonsai-client -f -interval 10 -name nas01 -port 1717 192.168.1.1:1717
Restart=always
RestartSec=5
StandardOutput=null
StandardError=null

[Install]
WantedBy=multi-user.target
```

Note that a `-f` instance is owned by systemd: starting a second client by hand
will never kill it (and `systemctl restart` is the way to change it).

### cron (no systemd)

`crontab -e`, then:

```cron
@reboot /usr/local/bin/bonsai-client 192.168.1.1
```

The client backgrounds itself, so cron does not need to.

### The server

The server is a terminal program, so run it in tmux (or screen) and reattach
later:

```console
$ tmux new -s bonsai './bonsai-server'
$ tmux attach -t bonsai
```

A systemd unit for it works too, as long as it gets a TTY:

```ini
[Unit]
Description=bonsai-monitor server
After=network-online.target

[Service]
ExecStart=/usr/local/bin/bonsai-server
Restart=always
TTYPath=/dev/tty10
StandardInput=tty
StandardOutput=tty
StandardError=tty
TTYReset=yes
TTYVHangup=yes

[Install]
WantedBy=multi-user.target
```

---

## Watching over SSH and tmux

This is the intended habitat, so it is designed for it:

- **Plain ASCII by default.** No exotic Unicode, no mouse, no bracketed-paste
  tricks. `-unicode` opts into richer glyphs if you want them.
- **Colour is auto-detected** and falls back to monochrome on a `vt100`-class
  terminal, so it is legible over a serial console too.
- **Resize is safe.** The tree is re-composed for the new size (scaled or
  cropped to fit), and a storm of resize events does not crash it or corrupt
  the display. Try it: drag the window, or `C-b :` then `resize-window` in
  tmux.
- **Small terminals work.** The canvas needs roughly 60×20 to look right; below
  that it crops rather than garbles.
- Nothing is ever written to the screen except the tree — logs go to a file.

If you are on a machine with no terminal at all (CI, a cron job, a container
with no TTY), the server refuses to start and tells you what to do instead:

```
bonsai-server: cannot initialise terminal (open /dev/tty: no such device or
address); try -dump N for a headless render
```

Port conflicts are reported the same way — **before** the terminal is touched,
so you never lose your screen to an error message:

```
bonsai-server: cannot listen on 0.0.0.0:1717: listen tcp 0.0.0.0:1717: bind: address already in use
```

---

## Headless mode (no terminal, no problem)

`-dump` renders N frames to stdout as plain ASCII and exits. It is how you
screenshot the tree, watch it in CI, or drive it from a script:

```console
$ ./bonsai-server -dump 12 -dump-interval 1s -dump-width 100 -dump-height 30
--- frame 1/12  13:52:46  network 50% (ok)  growth 0.85 ---
<the tree>
```

| Flag | Meaning |
| --- | --- |
| `-dump N` | print N frames and exit |
| `-dump-interval D` | spacing between frames (default 1 s) |
| `-dump-width`, `-dump-height` | canvas size (default 100×32) |
| `-dump-overlay hosts\|help` | start with the `l` or `?` overlay on |
| `-dump-growth F` | start at this growth (0–1) to preview tree sizes |
| `-show-config` | print the effective config as JSON and exit |

Each frame is preceded by a machine-readable header carrying network vitality,
the word and the growth value — handy for asserting on the tree in a test.

---

## The simulator

`cmd/simulate` drives a server with N fake clients so you can watch the whole
story without owning eight machines:

```console
$ go run ./cmd/simulate -server localhost -n 8
simulate: 8 fake clients -> 127.0.0.1:1717 (udp) every 2.0s
simulate: hosts: sim-01 sim-02 sim-03 sim-04 sim-05 sim-06 sim-07 sim-08
[13:52:47] phase 1/4: all healthy - the tree grows toward a full canopy (40s)
[13:53:27] phase 2/4: one host degrades - a red cluster, the tree dulls slightly (30s)
[13:53:57] phase 3/4: several degrade and one goes silent - droop, thin canopy, red and brown (45s)
[13:54:42] phase 4/4: recovery - the red clears and the tree regrows (50s)
```

The scripted roles are `healthy`, `cpu-spike`, `hot`, `flapping`, `silent` and
`rebooted`, and the timeline loops by default. Flags: `-server`, `-port`, `-n`,
`-interval`, `-tcp`, `-once` (run the timeline once and exit), `-static`
(everything healthy, no script), `-prefix`, `-seed` (reproducible), `-speed`
(>1 runs the story faster — useful with `-dump`), `-quiet`.

A real run, measured from `-dump` frame headers:

| phase | network | word | growth | canopy (leaf chars/frame) | labels |
| --- | --- | --- | --- | --- | --- |
| all healthy | 81 % | thriving | 0.85 → 0.88 | 164 → 194 | none |
| one host degrades | 49 % | stressed | 0.88 → 0.85 | 185 → 150 | `sim-08` only |
| several + one silent | 27–34 % | stressed | 0.84 → 0.78 | 121 → 95 | `sim-05/06/07/08` |
| recovery | 81 % | thriving | 0.78 → 0.82 | 151 → 166 | none |

One sick host dulls the whole tree and lights exactly one red cluster; several
sick hosts change the entire silhouette.

---

## Protocol

Base: **RFC 865 (QOTD)**, on port **1717** by default, on **both TCP and UDP**
at once. QOTD is normally one-way — connect, receive a quote, close, anything
you sent is discarded. This keeps that shape but *reads* your data as metrics
and still answers with a quote. Anything that speaks plain QOTD (`nc host 1717`)
gets exactly what it expects.

### Client → server

One line of space-separated `key=value` pairs, UTF-8, **max 512 bytes**,
newline-terminated:

```
host=nas01 cpu=12.5 temp=54.2 load=0.42 up=86400
```

| Key | Meaning | Unit |
| --- | --- | --- |
| `host` | hostname (**required**) | must match `[A-Za-z0-9._-]{1,32}` |
| `cpu` | CPU usage | percent, 0–100 |
| `temp` | CPU temperature | degrees C |
| `load` | 1-minute load average ÷ core count | float |
| `up` | uptime | seconds |

Everything except `host` is optional. Unknown keys are ignored. Values are
clamped into range, and `NaN`/`Inf` are rejected rather than clamped.

### Server → client

A rotating bonsai proverb (18 built in), optionally followed by one status
line. Always newline-terminated, always under 512 bytes:

```
The patient gardener outlives the storm.
status=healthy
```

`status=` is the reporting host's own band (`healthy`, `warning`, `critical`),
so a client can tell how the server graded it. A peer that sent nothing usable
gets the quote alone — no status line, nothing recorded.

### TCP

Accept → wait about 1 s for a payload line → parse it if one arrived → reply →
close. Only the **first line** of a connection is a payload. Before closing,
the server drains whatever else you had in flight, so a client that piped in a
second line still receives its quote instead of a connection reset.

### UDP

Each datagram is one payload; the reply goes back to the sender. An empty
datagram just gets a quote.

### Limits

| Protection | Value |
| --- | --- |
| payload cap | 512 bytes, on every read path |
| reply cap | 512 bytes |
| per-source rate limit | ~1 packet/second (extras are ignored, not queued) |
| host table cap | 256 hosts, oldest-seen evicted |
| hostname charset | `[A-Za-z0-9._-]{1,32}`, enforced before anything else |
| panics | every handler is wrapped in `recover()` and logged |

The rate limiter keys on `ip|host` for valid payloads and on `ip` alone for
garbage, so one flooding host cannot rewrite the tree and cannot starve its
neighbours. Malformed input is dropped, answered with a plain quote, and
optionally logged. **It never crashes the server.**

---

## Health scoring

Each metric is graded continuously: 1.0 at or below *healthy*, 0.0 at or beyond
*critical*, linear in between.

| Metric | Healthy | Critical |
| --- | --- | --- |
| `cpu` (%) | below 70 | above 90 |
| `temp` (°C) | below 65 | above 80 |
| `load` (per core) | below 0.8 | above 1.5 |

**Host vitality = the minimum of its metric scores.** Your worst metric defines
you; a machine cannot average away one pegged CPU.

**Uptime** is informational, except that a recent reboot (uptime under
5 minutes) caps that host's vitality at 0.6 — a machine that just came back is
not yet to be trusted.

**Staleness.** A host seen within 3× the report interval is fresh. After that
its vitality decays linearly toward 0 over 5 minutes; past 5 minutes it is
stale (brown, bare). A host gone for over an hour is removed, and regrows if it
returns.

**Network vitality** — the single number that drives the tree:

```
network = 0.6 * mean(host vitality) + 0.4 * min(host vitality)
```

The mean makes every machine contribute; the min term stops one dying host from
being averaged away, so a single critical machine visibly dulls the whole tree.
With zero hosts, network vitality is a neutral 0.5 and you get a seedling.

**Words:** ≥ 0.8 `thriving`, ≥ 0.5 `ok`, ≥ 0.25 `stressed`, below that
`ailing`.

---

## Tuning thresholds

Every threshold, decay window, weight and word cutoff lives in one config
struct with built-in defaults. To change any of them, write a JSON file with
**only the keys you want to override** and pass `-config`:

```console
$ ./bonsai-server -show-config > my.json      # start from the effective config
$ $EDITOR my.json
$ ./bonsai-server -config my.json
```

For example, a fleet of noisy Pis that run hot:

```json
{
  "temp": { "healthy": 70, "critical": 85 },
  "report_interval_seconds": 30,
  "growth_per_minute": 0.2,
  "vitality_words": [
    { "min": 0.85, "word": "thriving" },
    { "min": 0.6,  "word": "ok" },
    { "min": 0.3,  "word": "stressed" },
    { "min": 0,    "word": "ailing" }
  ]
}
```

| Key | Default | Meaning |
| --- | --- | --- |
| `bind`, `port` | `0.0.0.0`, `1717` | listen address |
| `max_payload_bytes` | 512 | hard cap on payloads and replies |
| `max_hosts` | 256 | host table cap, oldest-seen evicted |
| `rate_per_second` | 1 | per-source packet rate limit |
| `tcp_timeout_seconds` | 1 | how long TCP waits for a payload |
| `report_interval_seconds` | 10 | what clients are expected to use |
| `fresh_window_seconds` | 0 | 0 = derived, 3× the report interval |
| `decay_window_seconds` | 300 | staleness decay time |
| `evict_after_seconds` | 3600 | when a silent host is forgotten |
| `reboot_window_seconds` | 300 | "recently rebooted" window |
| `reboot_cap` | 0.6 | vitality cap during that window |
| `cpu`, `temp`, `load` | see above | `{healthy, critical}` per metric |
| `mean_weight`, `min_weight` | 0.6, 0.4 | the network formula (renormalised) |
| `empty_vitality` | 0.5 | seedling vitality with zero hosts |
| `host_green_above`, `host_yellow_above` | 0.75, 0.4 | cluster colour bands |
| `vitality_words` | 4 bands | vitality → word cutoffs |
| `growth_per_minute` | 0.11 | canopy growth rate while healthy |
| `dieback_per_minute` | 0.16 | canopy loss rate while sick |
| `growth_full_above` | 0.8 | vitality above which the tree grows |
| `growth_pause_below` | 0.5 | vitality below which it dies back |
| `seed_growth` | 0.08 | growth a fresh seedling starts at |
| `display_tau_seconds` | 3 | smoothing time constant for the display |
| `render_interval_seconds` | 1 | snapshot/draw cadence |
| `unicode` | false | richer glyphs |
| `log_path` | `""` | empty = `~/.bonsai-server.log` |

Flags override the file: `-port`, `-bind`, `-unicode`, `-log path`
(`-log none` disables logging entirely).

---

## Manual tests

Everything below is safe to run against a live server and is what the
acceptance criteria are checked with.

### The three protocol checks (from the spec)

```console
$ printf 'host=test cpu=95 temp=85 load=2.0 up=100\n' | nc -u -w1 SERVER 1717
Prune what is dying so the rest may live.
status=critical

$ printf 'host=test cpu=10\n' | nc -w1 SERVER 1717
A small tree, deeply rooted, outlasts a tall one.
status=healthy

$ nc -w1 SERVER 1717          # plain QOTD: no payload, just a quote
Every branch remembers the hand that shaped it.
```

The third one must not change anything: no host is recorded, the tree is
untouched. An empty UDP datagram behaves the same way.

### Garbage must never crash it

Throw anything you like at port 1717 — the server stays up, answers with a
quote, and records nothing:

```console
$ head -c 4096 /dev/urandom | nc -u -w1 SERVER 1717      # random binary
$ python3 -c 'print("host="+"a"*60000)' | nc -u -w1 SERVER 1717   # oversize
$ printf 'host=; rm -rf / cpu=1\n' | nc -u -w1 SERVER 1717        # injection
$ printf 'host=$(whoami) cpu=`id`\n' | nc -u -w1 SERVER 1717      # substitution
$ printf 'host=../../etc/passwd\n' | nc -u -w1 SERVER 1717        # traversal
$ printf 'host=%%s%%s%%n%%n cpu=%%d\n' | nc -u -w1 SERVER 1717     # format string
$ printf 'host=bad\xff\xfe cpu=1\n' | nc -u -w1 SERVER 1717       # invalid UTF-8
$ printf 'host=a cpu=NaN temp=+Inf load=-Inf\n' | nc -u -w1 SERVER 1717
$ printf 'host=a\nhost=b\ncpu=1\n' | nc -u -w1 SERVER 1717        # multi-line
$ head -c 70000 /dev/zero | nc -w1 SERVER 1717                    # TCP flood
```

Each of these returns a bare quote (no `status=` line) and leaves `hosts N`
unchanged in the status line. Numbers that are merely out of range
(`cpu=-500`, `temp=9999`) are *clamped* and accepted; `NaN` and `Inf` are
rejected outright. Nothing is ever passed to a shell, a template or
`strconv`-adjacent code that could interpret it.

### Resize

With the server in the foreground, resize the terminal window (or, in tmux:
`C-b :resize-window -A`, or split and unsplit panes). The tree re-composes for
the new size and keeps running. A rapid burst of resize events is fine — the
render loop coalesces them and draws at most about once per second.

### Client lifecycle

```console
$ ./bonsai-client SERVER                 # prints one line, prompt returns at once
$ ./bonsai-client -status                # running (pid N) -> SERVER:1717
$ ./bonsai-client SERVER                 # again: exactly one instance, newest wins
$ ./bonsai-client -status                # the new pid
$ kill -9 $(cat ${XDG_RUNTIME_DIR:-/tmp}/bonsai-client*.pid)
$ ./bonsai-client -status                # not running   (exit 1)
$ ./bonsai-client SERVER                 # starts cleanly despite the stale pidfile
$ ./bonsai-client -stop                  # stopped
```

Closing the SSH session that started the client leaves it running; a server
outage leaves it running and silent, and the host reappears in the tree when
the server returns.

### Unit tests

```console
$ go test ./...
ok  bonsai-monitor/client
ok  bonsai-monitor/server
```

Covered: payload parsing and validation (garbage, oversize, injection, NaN/Inf,
bad UTF-8), per-metric scoring, host vitality, the network aggregate (including
one critical host among healthy ones pulling the tree down), staleness decay,
the reboot cap, config normalisation and JSON override, tree growth and dieback,
leaf-cluster placement stability, canvas clipping, rendering under a fake
screen, and client pidfile handling (live, stale, recycled PID, zombie) plus
the orphan sweep.

---

## Troubleshooting

| Symptom | Where to look |
| --- | --- |
| Tree shows no hosts | Is the client running? `./bonsai-client -status`. Can it reach the port? `nc -u -w1 SERVER 1717` |
| Client says it started but nothing appears | `~/.bonsai-client.log` — send errors are logged there, never to your terminal |
| `address already in use` | Something else owns 1717 (`ss -lupn 'sport = :1717'`), or a second server is running. Use `-port` |
| Server refuses to start | It needs a real terminal. Use tmux, or `-dump N` for headless output |
| A host is brown and bare | It has not reported for >5 min: stale. It is forgotten after an hour |
| Tree is monochrome | Your `TERM` does not advertise colour (`vt100`, `dumb`). Expected, and still readable |
| Screen looks wrong after a resize | Press `^L` to force a redraw |
| Client won't stop | `-stop` only signals a process whose `/proc/<pid>/cmdline` is really `bonsai-client`; if it says `not running`, the pidfile is stale and was already cleaned up |

Log files: `~/.bonsai-server.log` and `~/.bonsai-client.log`. Both are
file-only — the screen is for the tree. `-log none` disables server logging.

---

## Repository layout

```
bonsai-monitor/
  go.mod                   Go 1.21, one dependency (tcell v2)
  Makefile                 build, test, cross-compile into dist/
  README.md
  server/
    main.go                flags, wiring, clean shutdown
    net.go                 UDP + TCP listeners, rate limiting, reply writing
    protocol.go            payload parsing, validation, quote rotation
    health.go              host vitality, staleness, network aggregate
    tree.go                the tree model: growth, dieback, leaf clusters
    render.go              tcell drawing, canvas, overlays, status line
    config.go              every default, plus the JSON override
    *_test.go
  client/
    main.go                the whole client: flags, daemonising, metrics, pidfile
    main_test.go
  cmd/simulate/main.go     scripted fake clients
  dist/                    cross-compiled static binaries (make dist)
```

Design constraints that shaped the code: the server may use exactly one
direct third-party package (`tcell`); the client uses the standard library only;
everything builds `CGO_ENABLED=0` and static; and the network path never blocks
on rendering (the draw loop works from a snapshot taken about once a second,
under a mutex).

---

## Scope

Everything in the acceptance criteria is implemented. Deliberately **not**
implemented, because they were listed as stretch goals:

- `-headless` server mode with a `bonsai-view` binary attaching over a Unix
  socket (multiple SSH sessions watching one server). `-dump` covers the
  no-terminal case, and tmux covers the multiple-viewers case.
- Persisting tree state across restarts. The tree starts as a seedling and
  regrows over a few minutes; nothing is written to disk.
- Seasonal effects and falling-leaf animation.
