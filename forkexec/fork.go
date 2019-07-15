package forkexec

import (
	"syscall"
	"unsafe" // required for go:linkname.

	"golang.org/x/sys/unix"
)

//go:linkname beforeFork syscall.runtime_BeforeFork
func beforeFork()

//go:linkname afterFork syscall.runtime_AfterFork
func afterFork()

//go:linkname afterForkInChild syscall.runtime_AfterForkInChild
func afterForkInChild()

// Start will fork, load seccomp and execv and being traced by ptrace
// Return pid and potential error
// Reference to src/syscall/exec_linux.go
// The runtime OS thread must be locked before calling this function
//go:noinline
//go:norace
func (r *Runner) Start() (int, error) {
	var (
		err1, err2  syscall.Errno
		r1          uintptr
		p           [2]int
		unshareUser = r.UnshareFlags&unix.CLONE_NEWUSER == unix.CLONE_NEWUSER
	)

	argv0, argv, envv, err := prepareExec(r.Args, r.Env)
	if err != nil {
		return 0, err
	}

	// prepare work dir
	workdir, err := syscallStringFromString(r.WorkDir)
	if err != nil {
		return 0, err
	}

	// prepare pivot_root param
	pivotRoot, oldRoot, err := preparePivotRoot(r.PivotRoot)
	if err != nil {
		return 0, err
	}

	// prepare mount param
	mountParams, dirsToMake, err := prepareMounts(r.Mounts)
	if err != nil {
		return 0, nil
	}

	if unshareUser {
		err = unix.Pipe2(p[:], unix.O_CLOEXEC)
		if err != nil {
			return 0, err
		}
	}

	// similar to exec_linux, avoid side effect by shuffling around
	fd, nextfd := prepareFds(r.Files)

	// Acquire the fork lock so that no other threads
	// create new fds that are not yet close-on-exec
	// before we fork.
	syscall.ForkLock.Lock()

	// About to call fork.
	// No more allocation or calls of non-assembly functions.
	beforeFork()

	// UnshareFlags (new namespaces) is activated by clone syscall
	pid, _, err1 := syscall.RawSyscall6(syscall.SYS_CLONE, uintptr(syscall.SIGCHLD)|(r.UnshareFlags&UnshareFlags), 0, 0, 0, 0, 0)
	if err1 != 0 || pid != 0 {
		// restore all signals
		afterFork()

		// synchronize with child for uid / gid map
		if unshareUser {
			unix.Close(p[0])
			err = writeIDMaps(int(pid))
			if err != nil {
				err2 = err.(syscall.Errno)
			}
			syscall.RawSyscall(syscall.SYS_WRITE, uintptr(p[1]), uintptr(unsafe.Pointer(&err2)), uintptr(unsafe.Sizeof(err2)))
			unix.Close(p[1])
		}

		syscall.ForkLock.Unlock()
		if err1 != 0 {
			return int(pid), syscall.Errno(err1)
		}
		return int(pid), nil
	}

	// In child process
	afterForkInChild()
	// Notice: cannot call any GO functions beyond this point

	// If usernamespace is unshared, uid map and gid map is required to create folders
	// and files
	// We need parent to setup uid_map / gid_map for us since we do not have capabilities
	// in the original namespace
	// At the same time, socket pair / pipe sychronization is required as well
	if unshareUser {
		if _, _, err1 = syscall.RawSyscall(syscall.SYS_CLOSE, uintptr(p[1]), 0, 0); err1 != 0 {
			goto childerror
		}
		r1, _, err1 = syscall.RawSyscall(syscall.SYS_READ, uintptr(p[0]), uintptr(unsafe.Pointer(&err2)), unsafe.Sizeof(err2))
		if err1 != 0 {
			goto childerror
		}
		if r1 != unsafe.Sizeof(err2) {
			err1 = syscall.EINVAL
			goto childerror
		}
		if err2 != 0 {
			err1 = err2
			goto childerror
		}
		if _, _, err1 = syscall.RawSyscall(syscall.SYS_CLOSE, uintptr(p[0]), 0, 0); err1 != 0 {
			goto childerror
		}
	}

	// Get pid of child
	pid, _, err1 = syscall.RawSyscall(syscall.SYS_GETPID, 0, 0, 0)
	if err1 != 0 {
		goto childerror
	}

	// Pass 1 & pass 2 assigns fds for child process
	// Pass 1: fd[i] < i => nextfd
	for i := 0; i < len(fd); i++ {
		if fd[i] >= 0 && fd[i] < int(i) {
			_, _, err1 = syscall.RawSyscall(syscall.SYS_DUP3, uintptr(fd[i]), uintptr(nextfd), 0)
			if err1 != 0 {
				goto childerror
			}
			// Set up close on exec
			syscall.RawSyscall(syscall.SYS_FCNTL, uintptr(nextfd), syscall.F_SETFD, syscall.FD_CLOEXEC)
			fd[i] = nextfd
			nextfd++
		}
	}

	// Pass 2: fd[i] => i
	for i := 0; i < len(fd); i++ {
		if fd[i] == -1 {
			syscall.RawSyscall(syscall.SYS_CLOSE, uintptr(i), 0, 0)
			continue
		}
		if fd[i] == int(i) {
			// dup2(i, i) will not clear close on exec flag, need to reset the flag
			_, _, err1 = syscall.RawSyscall(syscall.SYS_FCNTL, uintptr(fd[i]), syscall.F_SETFD, 0)
			if err1 != 0 {
				goto childerror
			}
			continue
		}
		_, _, err1 = syscall.RawSyscall(syscall.SYS_DUP3, uintptr(fd[i]), uintptr(i), 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// Set the pgid, so that the wait operation can apply to only certain
	// subgroup of processes
	_, _, err1 = syscall.RawSyscall(syscall.SYS_SETPGID, 0, 0, 0)
	if err1 != 0 {
		goto childerror
	}

	// If mount point is unshared, mark root as private to avoid propagate
	// outside to the original mount namespace
	if r.UnshareFlags&syscall.CLONE_NEWNS == syscall.CLONE_NEWNS {
		_, _, err1 = syscall.RawSyscall6(syscall.SYS_MOUNT, uintptr(unsafe.Pointer(&none[0])),
			uintptr(unsafe.Pointer(&slash[0])), 0, syscall.MS_REC|syscall.MS_PRIVATE, 0, 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// mount tmpfs & chdir to new root before performing mounts
	if pivotRoot != nil {
		// mount("tmpfs", root, "tmpfs", 0, "")
		_, _, err1 = syscall.RawSyscall6(syscall.SYS_MOUNT, uintptr(unsafe.Pointer(&tmpfs[0])),
			uintptr(unsafe.Pointer(pivotRoot)), uintptr(unsafe.Pointer(&tmpfs[0])), 0,
			uintptr(unsafe.Pointer(&empty[0])), 0)
		if err1 != 0 {
			goto childerror
		}

		_, _, err1 = syscall.RawSyscall(syscall.SYS_CHDIR, uintptr(unsafe.Pointer(pivotRoot)), 0, 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// performing mounts
	for i, m := range mountParams {
		// mkdirs(target)
		for _, p := range dirsToMake[i] {
			_, _, err1 = syscall.RawSyscall(syscall.SYS_MKDIRAT, uintptr(_AT_FDCWD), uintptr(unsafe.Pointer(p)), 0755)
			if err1 != 0 && err1 != syscall.EEXIST {
				goto childerror
			}
		}
		// mount(source, target, fsType, flags, data)
		_, _, err1 = syscall.RawSyscall6(syscall.SYS_MOUNT, uintptr(unsafe.Pointer(m.Source)),
			uintptr(unsafe.Pointer(m.Target)), uintptr(unsafe.Pointer(m.FsType)), uintptr(m.Flags),
			uintptr(unsafe.Pointer(m.Data)), 0)
		if err1 != 0 {
			goto childerror
		}
		// bind mount is not respect ro flag so that read-only bind mount needs remount
		if m.Flags&bindRo == bindRo {
			_, _, err1 = syscall.RawSyscall6(syscall.SYS_MOUNT, uintptr(unsafe.Pointer(&empty[0])),
				uintptr(unsafe.Pointer(m.Target)), uintptr(unsafe.Pointer(m.FsType)),
				uintptr(m.Flags|syscall.MS_REMOUNT), uintptr(unsafe.Pointer(m.Data)), 0)
			if err1 != 0 {
				goto childerror
			}
		}
	}

	// pivit_root
	if pivotRoot != nil {
		// mkdir(root/old_root)
		_, _, err1 = syscall.RawSyscall(syscall.SYS_MKDIRAT, uintptr(_AT_FDCWD), uintptr(unsafe.Pointer(oldRoot)), 0755)
		if err1 != 0 {
			goto childerror
		}

		// pivot_root(root, root/old_root)
		_, _, err1 = syscall.RawSyscall(syscall.SYS_PIVOT_ROOT, uintptr(unsafe.Pointer(pivotRoot)), uintptr(unsafe.Pointer(oldRoot)), 0)
		if err1 != 0 {
			goto childerror
		}

		// umount(root/old, MNT_DETACH)
		_, _, err1 = syscall.RawSyscall(syscall.SYS_UMOUNT2, uintptr(unsafe.Pointer(oldRoot)), syscall.MNT_DETACH, 0)
		if err1 != 0 {
			goto childerror
		}

		// rmdir(root/old_root)
		_, _, err1 = syscall.RawSyscall(syscall.SYS_UNLINKAT, uintptr(_AT_FDCWD), uintptr(unsafe.Pointer(oldRoot)), uintptr(unix.AT_REMOVEDIR))
		if err1 != 0 {
			goto childerror
		}
	}

	// chdir for child
	if workdir != nil {
		_, _, err1 = syscall.RawSyscall(syscall.SYS_CHDIR, uintptr(unsafe.Pointer(workdir)), 0, 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// Set limit
	for _, rlim := range r.RLimits {
		// Prlimit instead of setrlimit to avoid 32-bit limitation (linux > 3.2)
		_, _, err1 = syscall.RawSyscall6(syscall.SYS_PRLIMIT64, 0, uintptr(rlim.Res), uintptr(unsafe.Pointer(&rlim.Rlim)), 0, 0, 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// No new privs
	if r.NoNewPrivs || r.Seccomp != nil {
		_, _, err1 = syscall.RawSyscall6(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0, 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// Drop all capabilities
	if r.DropCaps {
		_, _, err1 = syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&dropCapHeader)), uintptr(unsafe.Pointer(&dropCapData)), 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// Enable Ptrace
	if r.Ptrace {
		_, _, err1 = syscall.RawSyscall(syscall.SYS_PTRACE, uintptr(syscall.PTRACE_TRACEME), 0, 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// if both seccomp and ptrace is defined, then seccomp filter should have
	// traced execve, thus child need parent attached to it first
	// actually, this is not effective if pid namespace is unshared
	if r.StopBeforeSeccomp || (r.Seccomp != nil && r.Ptrace) {
		// Stop to wait for ptrace tracer
		_, _, err1 = syscall.RawSyscall(syscall.SYS_KILL, pid, uintptr(syscall.SIGSTOP), 0)
		if err1 != 0 {
			goto childerror
		}
	}

	// Load seccomp, stop and wait for tracer
	if r.Seccomp != nil {
		// If execve is seccomp trapped, then tracee stop is necessary
		// otherwise execve will fail due to ENOSYS
		// Do getpid and kill to send SYS_KILL to self
		// need to do before seccomp as these might be traced

		// Load seccomp filter
		_, _, err1 = syscall.RawSyscall(unix.SYS_SECCOMP, SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(r.Seccomp)))
		if err1 != 0 {
			goto childerror
		}
	}

	// at this point, runner is successfully attached for seccomp trap filter
	// or execve traped without seccomp filter
	// time to exec
	_, _, err1 = syscall.RawSyscall(syscall.SYS_EXECVE,
		uintptr(unsafe.Pointer(argv0)),
		uintptr(unsafe.Pointer(&argv[0])),
		uintptr(unsafe.Pointer(&envv[0])))

childerror:
	syscall.RawSyscall(syscall.SYS_EXIT, uintptr(err1), 0, 0)
	// cannot reach this point
	panic("cannot reach")
}