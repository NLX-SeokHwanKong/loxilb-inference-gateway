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

## Separate follow-up

`ProcessInfoGet` still launches `top` and `ps`. It remains susceptible to the same child-reaper race and may provide incomplete process data; its command-error handling and output parsing need their own correction. This change does not fix that endpoint, modify the global reaper or claim that every status endpoint is subprocess-free.
