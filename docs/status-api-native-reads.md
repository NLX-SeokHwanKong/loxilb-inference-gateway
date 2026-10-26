# Native Linux device and filesystem status reads

Device and filesystem status reads use kernel files, `uname(2)` and `statfs(2)`. They do not spawn `uname` or `df`, so the process-wide SIGCHLD reaper cannot take those commands' exit statuses from `exec.Cmd.Wait`. The reaper runs regardless of whether the gateway is PID 1.

The existing response field names and types are retained, with these explicit value/selection semantics:

- `architecture` is now the kernel's machine architecture (`utsname.machine`), without a trailing newline. The former `uname -p` command reported a processor string and included a newline; processor and machine names need not match on every platform. Other device fields retain their existing formatting.
- Mount records are resolved by decoded mount path first: only the last stacked record at a path is considered. Otherwise `statfs(path)` would read the visible upper filesystem while labeling it with a covered lower mount's metadata.
- Visible aliases are grouped by device and filesystem type before probing. Prefer a filesystem-root mount to a bind alias. After a successful probe, skip its remaining aliases; if the preferred path disappeared or is inaccessible, another alias may be tried.
- Known kernel pseudo filesystems are omitted before `statfs`, including `autofs`, to avoid triggering automounts. Zero-block filesystems and unavailable paths are also omitted. If no usable filesystem remains, return an error rather than a fabricated empty healthy result.
- Block counts use `Frsize`; a missing/nonpositive fragment size falls back to `Bsize`. Usage is rounded upward from `used / (used + available)`, with `-` when that denominator is zero.
- Human-readable sizes round upward and promote units at rounding boundaries (for example 1023.5 KiB becomes `1.0M`, and 9.99 GiB becomes `10G`).

This is a kernel-backed usage report, not an exact GNU `df` clone. Nonzero-block storage filesystems such as tmpfs, devtmpfs, squashfs and FUSE are not blanket-filtered. Filesystem selection, alias preference, display and mount-race behavior can differ from a particular `df` version or invocation. In particular, an empty usage result does not prove there are no mounts.

## Automated validation

The `Status API contracts` workflow runs the status package with the Linux race detector. It also starts a separate private mount namespace and runs an opt-in two-layer tmpfs overmount regression. That fixture mounts only its own new temporary directory, disables mount propagation and unmounts both layers on return. No gateway or eBPF deployment is required.

The package tests cover stacked paths, escaped paths, bind aliases, skipped autofs/pseudo filesystems, disappearing mounts, fragment-size fallback, reserved blocks, undefined percentages, formatting boundaries and live syscall reads with no executable search path. The package is explicitly Linux-only.

## Native process status

`ProcessInfoGet` reads `/proc` directly and creates no `top`/`ps` children. All existing process response field names and string types are retained. CPU usage is cumulative user + system CPU time divided by elapsed process lifetime, multiplied by 100 (the former `ps` lifetime semantics, not a sampled interval); multithreaded CPU can exceed 100%. The kernel's `AT_CLKTCK` auxiliary-vector entry supplies the clock rate. Memory usage is resident bytes / `MemTotal`; virtual, resident and shared sizes are now consistently reported as decimal KiB strings rather than `top`'s adaptive units. CPU time is minutes:seconds.centiseconds. Command is the kernel comm name (normally limited by Linux to 15 bytes), preserving embedded spaces rather than splitting a `top` column. Effective UID resolves through the first matching `/etc/passwd` entry, falling back to its numeric string without NSS or subprocess calls. Rows sort by descending displayed CPU percentage, then ascending PID.

Processes that exit (including ESRCH between open and read), become inaccessible, or have incomplete/malformed per-PID records are skipped. A changed start time prevents combining records after PID reuse. Missing/invalid global procfs data and a wholly unreadable process list return a typed collection error and explicit HTTP 500, never message-inferred 400/404 or an empty healthy list; this also removes the former short-output slice panic. Procfs remains a sequence of kernel reads, not an atomic snapshot. Only priority -100 displays as `rt`; other negative priorities remain numeric. The global SIGCHLD reaper is unchanged.

Fixture tests cover fields, command delimiters, missing/corrupt procfs and UID fallback. A live Linux test makes 160 concurrent reads with an empty executable search path and requires the test process to be present in every response. These run in the existing Linux race-detector job.

The Linux race suite includes deterministic ESRCH injection at each per-PID read and live collection while six workers churn short-lived processes. Internal diagnostic causes are retained in typed errors but are not returned as raw procfs paths to API callers.
