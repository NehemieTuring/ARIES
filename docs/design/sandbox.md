# ToolSandbox

`ToolSandbox` owns the task environment. It starts one isolated environment for
a task, returns the narrow live capability needed by the selected bridge and
benchmark, and stops the environment with positive confirmation that owned
resources are absent.

## Boundary and lifecycle

The sandbox begins before bridge or harness startup and stays live through
independent benchmark evaluation. It does not own harness policy, tool
credentials, or benchmark scoring. Cleanup is idempotent, reverse-order, and
bounded after cancellation; failure to confirm resource absence remains a
cleanup failure.

The current implementations are Docker, through the Moby Go SDK, and Sandlock,
through `github.com/multikernel/sandlock/go`. The profile field `sandbox.type`
selects one of them with an explicit command switch. The default in every
checked-in profile remains `docker`. When `bin/aries` runs, `ARIES_SANDBOX_TYPE`
replaces that field. A value already in the process environment wins. Otherwise
ARIES reads only that variable from the repository-root `.env` beside `bin/`.
An empty value is rejected. Any other value still has to be `docker` or
`sandlock`; ARIES does not fall back to the profile.

Docker owns a task container and network. Sandlock does not start a task
container. Each command is a host process confined with Landlock, seccomp-bpf,
seccomp user notification, a filesystem policy, and a network allowlist.
Those controls are not cgroups or namespaces. The private directory is written
directly. Sandlock's copy-on-write branch is not used, because creates in that
branch are not visible to later lookups in the same command. `/usr`, `/var`,
and `/etc` are writable so a verifier can keep package state. The copied image
is readable, so a verifier can open reference files outside the workdir.
`/etc/shadow`, `/root`, and `/home` stay denied. The task process keeps the invoking user's
uid: this host's AppArmor profile for unconfined programs denies the
`sys_admin` capability required to map uid 0. `SHELL` is `/bin/sh` so an
installer that cannot read `/proc/self/exe` can still see an ELF binary. A
command is reaped when its process exits, even if the caller has not closed
standard input. Sandlock allows one live process for a sandbox name, so each command
receives its own name. Holding one name until stdin closes is still avoided:
the SSH bridge leaves its channel open until it sees an exit status.

Terminal-Bench still runs each task's own `/tests/test.sh`. Across the pinned
set of 89 scripts, 82 install uv 0.9.5 with curl and all 89 write
`/logs/verifier/reward.txt`. Sandlock keeps those scripts unchanged. It points
`UV_CACHE_DIR`, `UV_PYTHON_INSTALL_DIR`, `UV_TOOL_DIR`, `XDG_CACHE_HOME`,
and `PIP_CACHE_DIR` at directories under `/tmp`, sets `UV_NO_CONFIG=1` so
uv does not walk parent directories outside the task, puts
`PIP_USER=1` so pip does not need uid 0, and adds the task-local
`~/.local/bin` to `PATH`. Task-specific `apt-get install` of packages other
than what the image or the seed helper already contains still needs container
root and stays unsupported. `/home`, `/root`, and `/etc/shadow` stay denied.

Sandlock prepares a private directory and, in production, copies the task image
root filesystem into it with a helper container. That helper starts only to
install `curl` and the uv 0.9.5 binaries the verifier expects under the
task workdir when they are missing, then it is removed. Execution uses Sandlock only. The helper exists
because Terminal-Bench task files, the dynamic linker, and the verifier's
download tool live in the image; it is not the sandbox. Sandlock
resolves `/bin`, `/usr`, `/lib`, `/lib64`, and `/etc` inside that
private root. `/dev` is mapped from the host because tools need device nodes
such as `/dev/null` and those nodes cannot be copied. `/proc` stays the
copied image directory. Sandlock answers `readlink` of a non-symlink with
`ENOENT`, which makes glibc `realpath` fail, so each task loads
`/usr/lib/libaries-realpath.so` through `LD_PRELOAD`. That helper returns
`EINVAL` for ordinary files and directories and leaves real symlinks to
Sandlock. It also turns `mkdir` of a missing parent from `EACCES` into
`ENOENT`, which is what `create_dir_all` needs before it creates the rest
of a uv cache tree. The same helper runs a script by its shebang
interpreter, because Sandlock refuses to execute the script file itself. `/home`, `/root`, and `/etc/shadow` are denied. The rest of the
host filesystem is not mounted into the task.

`AllowNetwork: false` becomes an empty Sandlock allowlist, which denies
outbound traffic. `AllowNetwork: true` uses `*` only because Docker's
non-internal network is unrestricted. `sandbox.sandlock.net_allow` replaces
that mapping. `MaxCPU` is a percentage of one core, so ARIES maps a Docker CPU
value only when it is in `(0, 1]`. Larger CPU values are not capped to one
core. Memory and disk strings (`256M`) are Sandlock quotas, not cgroup bytes.

The existing SSH bridges still require a Docker network mode. Sandlock reports
network name `host` and gateway `127.0.0.1` so the harness container can reach
the bridge. Task processes do not join that network and do not receive the
host's unrestricted sockets.

```json
"sandbox": {
  "type": "sandlock",
  "sandlock": {
    "net_allow": ["api.openai.com:443", "github.com:443"],
    "max_processes": 64,
    "max_open_files": 256
  }
}
```

```sh
./bin/aries profiles/openclaw-tb2-fix-git-deepseek.json
```

A Sandlock run uses the same command with `"sandbox": {"type": "sandlock"}`,
or with `ARIES_SANDBOX_TYPE=sandlock` in the environment or the repository-root
`.env`.
The host must be Linux 6.12 or newer and must have `libsandlock_ffi` visible
to pkg-config. `make build` and `go test` link that library even when the
selected sandbox is Docker. The host also needs `gcc`: ARIES compiles
`libaries-realpath.so` with the host toolchain when a Sandlock sandbox starts.
That library matches the host libc. An image built on musl can fail to load it.
ARIES returns an error when Landlock is unavailable. It
does not fall back to Docker.

## Customization & Contribution Guide

A new sandbox implementation must preserve exact command argument boundaries,
context-aware external operations, bounded cancellation cleanup, and positive
absence checks. Put it in a concrete package with an explicit constructor and
command switch, then test partial startup, live evaluation, idempotent stop,
resource ownership, and bridge-facing capabilities. Update the supported
reference and operational prerequisites. Do not add registration, discovery,
factories, reflection, DI, or generic plugins.
