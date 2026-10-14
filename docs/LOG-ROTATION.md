# Log rotation & disk-usage protection

> Motivation: every log file the gateway wrote previously grew **unbounded**
> (and `--loglevel debug` was the default until it became `info`), which starves the disk on any long-lived
> production deployment.

## What writes logs, and how each is rotated now

| Producer | File(s) | Rotation mechanism |
|---|---|---|
| `tk.LogIt` (loxilib classic logger) | `/var/log/loxilb<HOSTNAME>.log` | in-process `pkg/logrotate.Writer` — loxilib opens a plain append file, so all level writers are re-pointed at the rotating writer at startup (`pkg/loxinet/loxinet.go`) |
| eBPF data-plane C library | `/var/log/loxilbdp.log` | the data-plane log's own writer thread renames the file to its backup name and opens a new one when the next write would pass the size limit (`log_rotate_fp` in `loxilb-ebpf/common/log.c`); the descriptor keeps its number, so nothing else notices. A sweeper (`logrotate.StartBackupSweeper`, 1-min interval) compresses and prunes the backups once they have been left alone for 10 s. No line is lost at a rotation |
| `pkg/loxilog` structured logs (CP+DP, json+text) | `/var/log/loxilb/loxilb-audit.json.log`, `loxilb<HOST>.log`, `loxilb-dp-audit.json.log`, `loxilb-dp<HOST>.log` | `logrotate.Writer` underneath the zerolog/diode pipeline |
| loxilb-mcp bridge audit trail | `<audit_dir>/audit.jsonl` | `logrotate.Writer`, fixed policy: 20 MB / 8 backups / 90 days / gzip (long forensic window, bounded disk) |
| process stdout (`LogTTY`) | container runtime log | **not rotated by loxilb** — see "Container deployments" below |

Rotated files are named `<base>-<UTC-timestamp>.log[.gz]` in the same directory,
e.g. `loxilb75e60c4d3de0-20260719-215610.875.log.gz`.

## Tuning knobs (loxilb)

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `--log-max-size` | `LOXILB_LOG_MAX_SIZE` | `50` | rotate when a file exceeds this many MB; `0` disables rotation |
| `--log-max-backups` | `LOXILB_LOG_MAX_BACKUPS` | `4` | rotated files kept per log, oldest deleted first (`0` = keep all until age limit) |
| `--log-max-age` | `LOXILB_LOG_MAX_AGE` | `28` | days to retain rotated files (`0` = keep forever) |
| `--log-no-compress` | `LOXILB_LOG_NO_COMPRESS` | off | disable gzip of rotated files |

Worst-case disk budget per log file ≈ `max-size × (1 + max-backups)` uncompressed —
with defaults, 250 MB per file *before* compression (gzip typically shrinks the
backups >10×; the live drill compressed 1 MB of debug logs to 44 KB).

## REST API interaction

- `GET /netlox/v1/log-archives` lists active + rotated logs from **both**
  `/var/log/` and `/var/log/loxilb/` (the structured-log directory was previously
  invisible to this API). Filter: name starts `loxilb`, ends `.log` / `.log.gz`.
- `GET /netlox/v1/log-archives/{filename}` downloads from either directory
  (path-traversal guarded).
- The MCP bridge exposes the same via `log_archives_list` / `log_archive_get`.
- The management audit trail is outside this API by construction: it lives in
  its own directory (`--audit-dir`, default `/var/log/loxilb/audit/`, mode
  0700) and its files are named `audit.jsonl` / `audit-<UTC>.jsonl[.gz]`, which
  the `loxilb*.log` filter never matches. A test holds that line. The trail's
  state is readable through `GET /netlox/v1/audit/status` — counters, the
  active segment, retention — never its records.

## Container deployments (important)

`tk.LogIt` also mirrors every line to **stdout** (`LogTTY=true`). When loxilb is
the container entrypoint, the container runtime captures that stream and its
growth is outside loxilb's control — configure the runtime side too:

- Docker: `--log-opt max-size=50m --log-opt max-file=4` (or daemon.json defaults).
- Kubernetes: kubelet `containerLogMaxSize` / `containerLogMaxFiles`.

In the cicd testbed loxilb is started via `docker exec`, so its stdout bypasses
the container log — production images that exec loxilb as PID 1 must set the
runtime limits above.

## Log calls never wait for the disk

A log line is not written by the thread that logs it. The data plane's worker
threads serve many connections each, and a log write that waits for a slow or
stalled device would hold every one of them, so:

- The data-plane log (`log_*` in C) copies each line into the calling thread's
  own in-memory ring (256 KiB); one writer thread writes stderr and
  `/var/log/loxilbdp.log`, and rotates the file.
- The gateway's log (`tk.LogIt`, and logrus) copies each line into an
  in-memory queue (8,192 lines); one goroutine writes
  `/var/log/loxilb<HOSTNAME>.log`, the stdout copy and stderr, and the file's
  rotation happens inside its write.

When the device is slower than the lines arrive, the ring or queue fills and
**lines are dropped, never requests held**. Dropped lines are counted in
`loxilb_log_lines_dropped_total{level}` (both logs together; `error`, `warn`,
`notice`, `info`, `debug`, `trace`, all reported from start at zero), and the
writer says so in the log once a second for each thread that dropped, when it
can write again. Alert on `rate(loxilb_log_lines_dropped_total[5m]) > 0`: it
means the log device cannot keep up, or the log level is too verbose for it.

Lines are written by their caller only during start-up, after shutdown has
stopped the writers (each is given 2 s to write what is queued), for
fatal-level lines, and in the fatal-signal handler, which also writes the
faulting worker's queued lines to stderr. After a hard kill (`SIGKILL`, power
loss) the lines still queued, normally the last few milliseconds, are lost.

At the default level (`info`) an admitted or refused AI request writes no log
line: it is in the audit trail and the metrics. The per-request lines are at
`debug`.

## Validation

- `pkg/logrotate` unit tests (`-race`): rotation trigger, gzip integrity,
  backup-count and age pruning, copytruncate sweep with a live foreign
  `O_APPEND` handle, the backup sweeper behind a writer that rotates for
  itself, disabled-config passthrough, concurrent writers.
- `make -C loxilb-ebpf/common test_log` and `go test -race ./pkg/logring`: a
  log call does not wait when the destination does not return; drops are
  counted by level and reported; nothing is lost across a rotation.
- End-to-end on a running gateway (`--log-max-size=1`): 1 MB of debug logs →
  timestamped `.log.gz` backup, active file reset mid-stream with zero process
  disruption; `GET /log-archives` listed it, and the download passed `gunzip -t`.

## Known limits / follow-ups

- `Sweep` / `StartSweeper` (copy-truncate, for a file another process appends to) is kept in `pkg/logrotate` but no longer used for the data-plane log.
- goBGP (`-b` mode) runs as a separate process with its own logging — not
  covered here; bound it via the container runtime limits.
- Stale per-hostname files from *previous* container identities (e.g. a
  `loxilb<oldhost>.log` baked into an image layer) are not auto-deleted; the
  age-based pruning only manages backups of the current process's logs.
