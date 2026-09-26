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
seccomp user notification, a filesystem policy, a network allowlist, and
copy-on-write on the task workdir. Those controls are not cgroups or namespaces. The private directory is written
directly. Sandlock's copy-on-write branch is not used, because creates in that
branch are not visible to later lookups in the same command.

Sandlock prepares a private directory and, in production, copies the task image
root filesystem into it with a helper container that is never started. Execution
then uses Sandlock only. The helper exists because Terminal-Bench task files
and the dynamic linker live in the image; it is not the sandbox. Sandlock
resolves `/bin`, `/usr`, `/lib`, `/lib64`, `/etc`, and `/proc` inside that
private root. `/dev` is the one host directory mapped into the chroot, because
tools need device nodes such as `/dev/null` and those nodes cannot be copied.
`/home`, `/root`, and `/etc/shadow` are denied. The rest of the host filesystem
is not mounted into the task.

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
to pkg-config. ARIES returns an error when that protection is unavailable. It
does not fall back to Docker.

## Customization & Contribution Guide

A new sandbox implementation must preserve exact command argument boundaries,
context-aware external operations, bounded cancellation cleanup, and positive
absence checks. Put it in a concrete package with an explicit constructor and
command switch, then test partial startup, live evaluation, idempotent stop,
resource ownership, and bridge-facing capabilities. Update the supported
reference and operational prerequisites. Do not add registration, discovery,
factories, reflection, DI, or generic plugins.
