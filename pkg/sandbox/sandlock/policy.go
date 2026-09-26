package sandlock

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	sandlocksdk "github.com/multikernel/sandlock/go"
)

// systemMounts are host directories visible inside the task chroot so dynamic
// loaders and ordinary tools can run. The rest of the host filesystem is not
// mounted.
var systemMounts = []string{"/usr", "/lib", "/lib64", "/bin", "/etc", "/proc"}

// Policy is the Sandlock configuration derived from one task. It is not a
// Docker container policy: Landlock paths, seccomp limits, and COW do not
// match cgroup or namespace semantics.
type Policy struct {
	Readable     []string
	Writable     []string
	Denied       []string
	Mounts       map[string]string
	NetAllow     []string
	Workdir      string
	Cwd          string
	Chroot       string
	MaxMemory    string
	MaxDisk      string
	MaxProcesses uint32
	MaxCPU       uint8
	MaxOpenFiles uint32
	NumCPUs      uint32
	GPUDevices   []uint32
	Env          map[string]string
}

func (p Policy) sandbox() sandlocksdk.Sandbox {
	return sandlocksdk.Sandbox{
		FSReadable:   append([]string(nil), p.Readable...),
		FSWritable:   append([]string(nil), p.Writable...),
		FSDenied:     append([]string(nil), p.Denied...),
		FSMount:      cloneMounts(p.Mounts),
		NetAllow:     append([]string(nil), p.NetAllow...),
		Workdir:      p.Workdir,
		Cwd:          p.Cwd,
		Chroot:       p.Chroot,
		MaxMemory:    p.MaxMemory,
		MaxDisk:      p.MaxDisk,
		MaxProcesses: p.MaxProcesses,
		MaxCPU:       p.MaxCPU,
		MaxOpenFiles: p.MaxOpenFiles,
		NumCPUs:      p.NumCPUs,
		GPUDevices:   append([]uint32(nil), p.GPUDevices...),
		CleanEnv:     true,
		Env:          cloneEnv(p.Env),
		OnExit:       sandlocksdk.BranchActionCommit,
		OnError:      sandlocksdk.BranchActionCommit,
	}
}

// taskPolicy maps a benchmark environment onto Sandlock.
//
// Network: an explicit allowlist wins. Otherwise AllowNetwork uses Sandlock's
// "*" rule, because Docker's non-internal network is unrestricted. A task
// with AllowNetwork false gets an empty allowlist, which Sandlock treats as
// deny-all. Nil would mean "no restriction", so the empty slice is deliberate.
//
// CPU: Docker NanoCPUs are a cgroup quota. Sandlock MaxCPU is a percentage of
// one core, so only a fraction in (0, 1] is mapped. Larger values are left
// unset rather than silently capped at one core. NumCPUs only changes the
// synthetic /proc/cpuinfo count.
func taskPolicy(root, workdir string, environment core.Environment, configured Options) (Policy, error) {
	readable := make([]string, 0, len(systemMounts))
	for _, path := range systemMounts {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		readable = append(readable, path)
	}
	if len(readable) == 0 {
		return Policy{}, fmt.Errorf("%w: none of the system paths exist", ErrUnavailable)
	}
	mounts := map[string]string{}
	if info, err := os.Stat("/dev"); err == nil && info.IsDir() {
		mounts["/dev"] = "/dev"
		readable = append(readable, "/dev")
	}
	writable := []string{workdir, "/tmp", "/logs", "/tests"}
	denied := []string{"/proc/kcore", "/etc/shadow", "/root", "/home"}
	denied = append(denied, configured.FSDenied...)
	netAllow := configured.NetAllow
	if netAllow == nil {
		if environment.AllowNetwork {
			netAllow = []string{"*"}
		} else {
			netAllow = []string{}
		}
	}
	policy := Policy{
		Readable: readable,
		Writable: writable,
		Denied:   denied,
		Mounts:   mounts,
		NetAllow: append([]string(nil), netAllow...),
		// Workdir enables Sandlock COW. A branch hides creates from later
		// lookups in the same command, so the private directory is written
		// directly instead. hostWork is still created by Start.
		Workdir:      "",
		Cwd:          workdir,
		Chroot:       root,
		MaxProcesses: configured.MaxProcesses,
		MaxOpenFiles: configured.MaxOpenFiles,
		Env:          taskEnvironment(environment.Env, workdir),
	}
	if environment.MemoryMB > 0 {
		policy.MaxMemory = fmt.Sprintf("%dM", environment.MemoryMB)
	}
	if environment.StorageMB > 0 {
		policy.MaxDisk = fmt.Sprintf("%dM", environment.StorageMB)
	}
	if configured.MaxCPUPercent > 0 {
		policy.MaxCPU = configured.MaxCPUPercent
	} else if environment.CPU > 0 && environment.CPU <= 1 && !math.IsNaN(environment.CPU) {
		policy.MaxCPU = uint8(environment.CPU * 100)
	}
	if environment.CPU > 1 && environment.CPU <= math.MaxUint32 && environment.CPU == math.Trunc(environment.CPU) {
		policy.NumCPUs = uint32(environment.CPU)
	}
	if environment.GPUs > 0 {
		policy.GPUDevices = make([]uint32, environment.GPUs)
		for index := range policy.GPUDevices {
			policy.GPUDevices[index] = uint32(index)
		}
	}
	return policy, nil
}

func taskEnvironment(values map[string]string, workdir string) map[string]string {
	env := map[string]string{
		"PATH":            "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME":            workdir,
		"TMPDIR":          "/tmp",
		"LANG":            "C.UTF-8",
		"DEBIAN_FRONTEND": "noninteractive",
	}
	if tz := os.Getenv("TZ"); tz != "" {
		env["TZ"] = tz
	} else {
		env["TZ"] = "UTC"
	}
	for key, value := range values {
		env[key] = value
	}
	return env
}

func commandEnvironment(base, overlay map[string]string) map[string]string {
	env := cloneEnv(base)
	for key, value := range overlay {
		env[key] = value
	}
	return env
}

func cloneEnv(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func cloneMounts(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func hostPath(root, containerPath string) (string, error) {
	if containerPath == "" || !filepath.IsAbs(containerPath) {
		return "", fmt.Errorf("path %q must be absolute", containerPath)
	}
	clean := filepath.Clean(containerPath)
	if clean != containerPath && !(containerPath == "/" && clean == "/") {
		return "", fmt.Errorf("path %q must be clean", containerPath)
	}
	if clean == "/" {
		return root, nil
	}
	return filepath.Join(root, strings.TrimPrefix(clean, "/")), nil
}
