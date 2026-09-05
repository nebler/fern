package taskenvdocker

import (
	"encoding/json"
	"strings"
)

// A deliberately restricted, fixed syscall allowlist for the worker (not an
// operator-configurable profile). In particular ioctl is NOT unrestricted:
// an unprivileged XFS inode owner in the initial user namespace can change its
// project ID and PROJINHERIT, even with ALL capabilities dropped. Only terminal
// and pipe queries are admitted. Unknown/new file-attribute APIs fail closed.
// The ordinary syscall names are a subset of Docker 28's default profile.
func workerSecurityOptions() []string {
	profile := map[string]any{
		"defaultAction": "SCMP_ACT_ERRNO", "defaultErrnoRet": 1,
		"syscalls": []map[string]any{
			{"names": strings.Fields(`accept accept4 access alarm arch_prctl bind brk capget capset chdir chmod chown clock_getres clock_gettime clock_nanosleep close close_range connect copy_file_range creat dup dup2 dup3 epoll_create epoll_create1 epoll_ctl epoll_pwait epoll_pwait2 epoll_wait eventfd eventfd2 execve execveat exit exit_group faccessat faccessat2 fadvise64 fallocate fchdir fchmod fchmodat fchmodat2 fchown fchownat fcntl fdatasync fgetxattr flistxattr flock fork fremovexattr fsetxattr fstat fstatfs fsync ftruncate futex futex_waitv futimesat getcpu getcwd getdents getdents64 getegid geteuid getgid getgroups getitimer getpeername getpgid getpgrp getpid getppid getpriority getrandom getresgid getresuid getrlimit get_robust_list getrusage getsid getsockname getsockopt gettid gettimeofday getuid getxattr inotify_add_watch inotify_init inotify_init1 inotify_rm_watch kill lchown lgetxattr link linkat listen listxattr llistxattr lremovexattr lseek lsetxattr lstat madvise membarrier memfd_create mincore mkdir mkdirat mlock mlock2 mlockall mmap mprotect mremap msync munlock munlockall munmap nanosleep newfstatat open openat openat2 pause pidfd_open pidfd_send_signal pipe pipe2 poll ppoll prctl pread64 preadv preadv2 prlimit64 pselect6 pwrite64 pwritev pwritev2 read readahead readlink readlinkat readv recvfrom recvmmsg recvmsg removexattr rename renameat renameat2 restart_syscall rmdir rseq rt_sigaction rt_sigpending rt_sigprocmask rt_sigqueueinfo rt_sigreturn rt_sigsuspend rt_sigtimedwait rt_tgsigqueueinfo sched_getaffinity sched_getattr sched_getparam sched_get_priority_max sched_get_priority_min sched_getscheduler sched_rr_get_interval sched_setaffinity sched_setattr sched_setparam sched_setscheduler sched_yield seccomp select sendfile sendmmsg sendmsg sendto setfsgid setfsuid setgid setgroups setitimer setpgid setpriority setregid setresgid setresuid setreuid setrlimit set_robust_list setsid setsockopt set_tid_address setuid setxattr shutdown sigaltstack signalfd signalfd4 socket socketpair splice stat statfs statx symlink symlinkat sync sync_file_range syncfs sysinfo tee tgkill time timer_create timer_delete timer_getoverrun timer_gettime timer_settime timerfd_create timerfd_gettime timerfd_settime times tkill truncate umask uname unlink unlinkat utime utimensat utimes vfork vmsplice wait4 waitid write writev`), "action": "SCMP_ACT_ALLOW"},
			// Docker's namespace-blocking clone mask; amd64/arm64 use arg 0.
			{"names": []string{"clone"}, "action": "SCMP_ACT_ALLOW", "args": []map[string]any{{"index": 0, "value": 2114060288, "valueTwo": 0, "op": "SCMP_CMP_MASKED_EQ"}}},
			{"names": []string{"clone3"}, "action": "SCMP_ACT_ERRNO", "errnoRet": 38},
		},
	}
	rules := profile["syscalls"].([]map[string]any)
	for _, request := range []uint64{0x5401, 0x5402, 0x5403, 0x5404, 0x540f, 0x5410, 0x5413, 0x5414, 0x541b, 0x5421, 0x5451} {
		rules = append(rules, map[string]any{"names": []string{"ioctl"}, "action": "SCMP_ACT_ALLOW", "args": []map[string]any{{"index": 1, "value": request, "op": "SCMP_CMP_EQ"}}})
	}
	profile["syscalls"] = rules
	data, err := json.Marshal(profile)
	if err != nil {
		panic(err)
	} // only fixed, JSON-compatible values above
	return []string{"no-new-privileges", "seccomp=" + string(data)}
}

func workerTmpfs() map[string]string {
	return map[string]string{
		"/tmp":                    "rw,nosuid,nodev,size=268435456,nr_inodes=65536,mode=1777",
		"/home/user/.cache":       "rw,nosuid,nodev,size=268435456,nr_inodes=65536,uid=1001,gid=1001,mode=0700",
		"/home/user/.config":      "rw,nosuid,nodev,size=16777216,nr_inodes=4096,uid=1001,gid=1001,mode=0700",
		"/home/user/.local/state": "rw,nosuid,nodev,size=16777216,nr_inodes=4096,uid=1001,gid=1001,mode=0700",
	}
}
