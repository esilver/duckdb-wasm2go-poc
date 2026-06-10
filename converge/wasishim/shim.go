// Package wasishim implements the libc / WASI host surface that a standalone
// emcc build (-sSTANDALONE_WASM -sFILESYSTEM=0) presents as imports beyond the
// C++ exception ABI. It is the second half of the env the wasm2go-generated
// module needs (the first half is exhost).
//
// Scope for an IN-MEMORY DuckDB `SELECT 1`, which does essentially no real I/O:
//
//	IMPLEMENTED (enough to open :memory:, run a scalar query, print):
//	  - emscripten_resize_heap   -> grow the module's exported memory
//	  - emscripten_memcpy_js     -> memmove within module memory
//	  - fd_write (wasi)          -> stdout/stderr to the host's writers
//	  - proc_exit (wasi)         -> record exit code, unwind via panic
//	  - random_get (wasi)        -> crypto/rand into module memory
//	  - clock_time_get (wasi)    -> real monotonic/realtime clock
//	  - environ_sizes_get/environ_get (wasi) -> empty environment
//	  - emscripten_get_now / _emscripten_date_now / time / clock_gettime helpers
//
//	STUBBED (return ENOSYS / 0 and LOG if a `SELECT 1` ever reaches them, so a
//	real DuckDB build tells us exactly what extra I/O it wants):
//	  - fd_read / fd_seek / fd_close / fd_sync / fd_fdstat_get (wasi)
//	  - path_open and the rest of the filesystem-backed wasi calls
//	  - the emscripten __syscall_* family (openat/stat/ioctl/...)
//	  - abort / __assert_fail (logged, then unwound)
//
// The shim needs to read/write the module's linear memory (to copy iovecs for
// fd_write, fill random_get, etc). It reaches memory through MemoryABI, which
// the run harness adapts from the generated *Module.
package wasishim

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"
)

// MemoryABI exposes the module's linear memory and heap growth to the shim.
type MemoryABI interface {
	// Mem returns the live backing slice of the wasm's exported memory.
	// (Live: it must reflect Grow, so callers re-fetch after growth.)
	Mem() []byte
	// Grow grows linear memory by deltaPages 64KiB pages, returning the old
	// page count or -1 on failure (wasm memory.grow semantics).
	Grow(deltaPages int32) int32
}

// ExitError is panicked by proc_exit/abort so the run harness can convert a
// wasm "process exit" into a Go error instead of killing the test binary.
type ExitError struct {
	Code   int32
	Reason string
}

func (e ExitError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("wasm exit code=%d (%s)", e.Code, e.Reason)
	}
	return fmt.Sprintf("wasm exit code=%d", e.Code)
}

// Shim implements the WASI/libc env methods. Construct with New.
type Shim struct {
	mu  sync.Mutex
	mem MemoryABI

	Stdout io.Writer
	Stderr io.Writer

	// Log captures every call into a STUBBED path, so a real DuckDB run reveals
	// the exact residual I/O surface it needs.
	Log []string

	startTime time.Time

	// Filesystem state (see fs.go). fds maps a WASI file descriptor to its open
	// entry; nextFd is the next descriptor to hand out; preopenRoot is the host
	// directory mapped to the single preopen at fd 3 (preopenFd). fsInit guards
	// lazy initialization so the table is set up on first FS use.
	fds         map[int32]*fdEntry
	nextFd      int32
	preopenRoot string
	preopenName string

	// Path-B host filesystem state (see hostfs.go). The Tier-2 "custom DuckDB
	// FileSystem" build (duckdb_fs.wasm) imports env.host_* functions that map a
	// small fd table directly to *os.File handles, bypassing the WASI seam above.
	hostFds    map[int32]*os.File
	hostNextFd int32
}

// New returns a Shim writing program output to stdout/stderr writers.
func New(mem MemoryABI, stdout, stderr io.Writer) *Shim {
	return &Shim{mem: mem, Stdout: stdout, Stderr: stderr, startTime: time.Now()}
}

// SetMem lets the run harness install the memory adapter after the module is
// constructed (the module does not exist when the env is first built).
func (s *Shim) SetMem(m MemoryABI) { s.mem = m }

func (s *Shim) logf(f string, a ...any) {
	s.Log = append(s.Log, fmt.Sprintf(f, a...))
}

func (s *Shim) memb() []byte { return s.mem.Mem() }

// WASI errno values used here.
const (
	wasiESUCCESS = 0
	wasiENOSYS   = 52
	wasiEBADF    = 8
)

// ---- emscripten env (heap + memcpy + time) --------------------------------

// emscripten_resize_heap(requestedBytes) grows linear memory to at least
// requestedBytes. Returns 1 on success, 0 on failure (emcc ABI).
func (s *Shim) Xemscripten_resize_heap(requested int32) int32 {
	cur := int32(len(s.memb()))
	if requested <= cur {
		return 1
	}
	const page = 64 * 1024
	need := requested - cur
	pages := (need + page - 1) / page
	if s.mem.Grow(pages) < 0 {
		s.logf("emscripten_resize_heap(%d) FAILED grow by %d pages", requested, pages)
		return 0
	}
	return 1
}

// emscripten_memcpy_js(dest, src, n) copies within linear memory. emcc lowers
// large memcpy to this import.
func (s *Shim) Xemscripten_memcpy_js(dest, src, n int32) int32 {
	mem := s.memb()
	copy(mem[dest:dest+n], mem[src:src+n])
	return dest
}

// emscripten_get_now returns a monotonic clock in milliseconds (float64).
func (s *Shim) Xemscripten_get_now() float64 {
	return float64(time.Since(s.startTime).Nanoseconds()) / 1e6
}

// _emscripten_date_now returns wall-clock milliseconds since the epoch.
func (s *Shim) X_emscripten_date_now() float64 {
	return float64(time.Now().UnixNano()) / 1e6
}

// _emscripten_get_now_is_monotonic: yes (1).
func (s *Shim) X_emscripten_get_now_is_monotonic() int32 { return 1 }

// emscripten_notify_memory_growth(index) is what an ALLOW_MEMORY_GROWTH=1 build
// imports instead of emscripten_resize_heap: the wasm grows its own memory with
// the memory.grow instruction (wasm2go handles that) and just NOTIFIES the host
// so JS could re-view the buffer. In pure Go the generated module re-slices its
// own memory, so this is a no-op hook.
func (s *Shim) Xemscripten_notify_memory_growth(index int32) {}

// ---- WASI snapshot preview1 -----------------------------------------------

// fd_write is implemented in fs.go (it keeps fd 1/2 -> stdout/stderr and routes
// other fds to the OS-backed file table).

// random_get(bufPtr, bufLen): fill module memory with cryptographic randomness.
func (s *Shim) Xrandom_get(bufPtr, bufLen int32) int32 {
	mem := s.memb()
	if _, err := rand.Read(mem[bufPtr : bufPtr+bufLen]); err != nil {
		s.logf("random_get failed: %v", err)
		return wasiENOSYS
	}
	return wasiESUCCESS
}

// clock_time_get(clockID, precision, timePtr): write current time in ns as i64.
func (s *Shim) Xclock_time_get(clockID int32, precision int64, timePtr int32) int32 {
	mem := s.memb()
	var ns int64
	switch clockID {
	case 1: // MONOTONIC
		ns = time.Since(s.startTime).Nanoseconds()
	default: // REALTIME and others
		ns = time.Now().UnixNano()
	}
	binary.LittleEndian.PutUint64(mem[timePtr:], uint64(ns))
	return wasiESUCCESS
}

// clock_res_get(clockID, resPtr): report 1ns resolution.
func (s *Shim) Xclock_res_get(clockID, resPtr int32) int32 {
	binary.LittleEndian.PutUint64(s.memb()[resPtr:], 1)
	return wasiESUCCESS
}

// hostEnviron is the environment exposed to the wasm. Deliberately MINIMAL:
// only HOME, which DuckDB's FileSystem::GetHomeDirectory falls back to when the
// home_directory setting is unset. Without it '~' expansion yields "" and
// ExtensionHelper::ExtensionDirectory trips over rfind(home,0)==0 being
// vacuously true for every path ('Cannot access directory ""'). We do NOT pass
// the full host environ so stray DUCKDB_*/TZ vars cannot perturb the engine.
func hostEnviron() []string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return []string{"HOME=" + home}
	}
	return nil
}

// environ_sizes_get(countPtr, bufSizePtr): the minimal HOME-only environment.
func (s *Shim) Xenviron_sizes_get(countPtr, bufSizePtr int32) int32 {
	env := hostEnviron()
	size := 0
	for _, kv := range env {
		size += len(kv) + 1
	}
	mem := s.memb()
	binary.LittleEndian.PutUint32(mem[countPtr:], uint32(len(env)))
	binary.LittleEndian.PutUint32(mem[bufSizePtr:], uint32(size))
	return wasiESUCCESS
}

// environ_get(environPtr, bufPtr): write the env strings NUL-terminated at
// bufPtr and the per-entry pointers at environPtr (WASI preview1 contract).
func (s *Shim) Xenviron_get(environPtr, bufPtr int32) int32 {
	mem := s.memb()
	off := bufPtr
	for i, kv := range hostEnviron() {
		binary.LittleEndian.PutUint32(mem[environPtr+int32(4*i):], uint32(off))
		copy(mem[off:], kv)
		mem[off+int32(len(kv))] = 0
		off += int32(len(kv)) + 1
	}
	return wasiESUCCESS
}

// args_sizes_get / args_get: empty argv.
func (s *Shim) Xargs_sizes_get(countPtr, bufSizePtr int32) int32 {
	mem := s.memb()
	binary.LittleEndian.PutUint32(mem[countPtr:], 0)
	binary.LittleEndian.PutUint32(mem[bufSizePtr:], 0)
	return wasiESUCCESS
}
func (s *Shim) Xargs_get(argvPtr, bufPtr int32) int32 { return wasiESUCCESS }

// proc_exit(code): a wasm "process exit". Unwind to the run harness.
func (s *Shim) Xproc_exit(code int32) {
	s.logf("proc_exit(%d)", code)
	panic(ExitError{Code: code, Reason: "proc_exit"})
}

// NOTE: the WASI filesystem-backed calls (fd_read, fd_pread, fd_seek, fd_close,
// fd_sync, fd_datasync, fd_tell, fd_fdstat_get, fd_fdstat_set_flags,
// fd_filestat_get, fd_prestat_get, fd_prestat_dir_name, fd_readdir, path_open,
// path_filestat_get, path_create_directory, path_unlink_file,
// path_remove_directory, path_rename, path_filestat_set_times) are implemented
// in fs.go, backed by the real OS via the os package.

// ---- STUBBED emscripten __syscall_* (FILESYSTEM=0 should not call these) ----

func (s *Shim) X__syscall_openat(dirFd, pathPtr, flags, mode int32) int32 {
	s.logf("STUB __syscall_openat -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_fcntl64(fd, cmd, varargs int32) int32 {
	s.logf("STUB __syscall_fcntl64 -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_ioctl(fd, op, varargs int32) int32 {
	s.logf("STUB __syscall_ioctl -> -ENOSYS")
	return -wasiENOSYS
}

// ---- implemented stat family (emscripten ABI) -------------------------------
// A handful of engine paths bypass the DuckDB FileSystem seam and issue raw
// libc calls; the load-bearing one is LocalFileSystem::IsPrivateFile (lstat on
// persistent-secret files, secret_manager read-back). Implemented against the
// host OS like host_* / getcwd.
//
// Emscripten errno numbering (musl-on-wasm uses the WASI codes; matches the
// -EINVAL/-ERANGE constants getcwd below already uses).
const (
	emEACCES  = 2
	emEEXIST  = 20
	emEINVAL  = 28
	emEIO     = 29
	emENOENT  = 44
	emENOTDIR = 54
)

// emErrnoOf maps a Go fs error onto emscripten's errno numbering (positive;
// callers negate).
func emErrnoOf(err error) int32 {
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.ENOTDIR {
		return emENOTDIR
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return emENOENT
	case errors.Is(err, fs.ErrExist):
		return emEEXIST
	case errors.Is(err, fs.ErrPermission):
		return emEACCES
	case errors.Is(err, fs.ErrInvalid):
		return emEINVAL
	default:
		return emEIO
	}
}

// cString reads a NUL-terminated string from module memory (libc pathnames).
func (s *Shim) cString(ptr int32) string {
	mem := s.memb()
	if ptr <= 0 || int(ptr) >= len(mem) {
		return ""
	}
	end := int(ptr)
	for end < len(mem) && mem[end] != 0 {
		end++
	}
	return string(mem[ptr:end])
}

// writeEmStat fills an emscripten `struct stat` (96 bytes; layout per
// musl/arch/emscripten/bits/stat.h: dev u32@0, mode u32@4, nlink u32@8,
// uid u32@12, gid u32@16, rdev u32@20, size i64@24, blksize i32@32,
// blocks i32@36, atim {sec i64, nsec i32}@40, mtim@56, ctim@72, ino u64@88).
func (s *Shim) writeEmStat(buf int32, fi os.FileInfo) {
	mem := s.memb()
	b := mem[buf : buf+96]
	for i := range b {
		b[i] = 0
	}
	le := binary.LittleEndian
	mode := uint32(fi.Mode().Perm())
	switch {
	case fi.Mode().IsDir():
		mode |= 0o040000 // S_IFDIR
	case fi.Mode()&fs.ModeSymlink != 0:
		mode |= 0o120000 // S_IFLNK
	default:
		mode |= 0o100000 // S_IFREG
	}
	var dev, ino uint64 = 1, 1
	var nlink, uid, gid uint32 = 1, 0, 0
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		dev, ino = uint64(st.Dev), uint64(st.Ino)
		nlink, uid, gid = uint32(st.Nlink), st.Uid, st.Gid
		mode = uint32(st.Mode) // exact bits incl. setuid/sticky
	}
	le.PutUint32(b[0:], uint32(dev))
	le.PutUint32(b[4:], mode)
	le.PutUint32(b[8:], nlink)
	le.PutUint32(b[12:], uid)
	le.PutUint32(b[16:], gid)
	le.PutUint64(b[24:], uint64(fi.Size()))
	le.PutUint32(b[32:], 4096)                        // st_blksize
	le.PutUint32(b[36:], uint32((fi.Size()+511)/512)) // st_blocks
	sec, nsec := uint64(fi.ModTime().Unix()), uint32(fi.ModTime().Nanosecond())
	le.PutUint64(b[40:], sec) // st_atim (mtime stands in)
	le.PutUint32(b[48:], nsec)
	le.PutUint64(b[56:], sec) // st_mtim
	le.PutUint32(b[64:], nsec)
	le.PutUint64(b[72:], sec) // st_ctim
	le.PutUint32(b[80:], nsec)
	le.PutUint64(b[88:], ino)
}

func (s *Shim) X__syscall_stat64(pathPtr, bufPtr int32) int32 {
	fi, err := os.Stat(s.cString(pathPtr))
	if err != nil {
		return -emErrnoOf(err)
	}
	s.writeEmStat(bufPtr, fi)
	return 0
}
func (s *Shim) X__syscall_lstat64(pathPtr, bufPtr int32) int32 {
	fi, err := os.Lstat(s.cString(pathPtr))
	if err != nil {
		return -emErrnoOf(err)
	}
	s.writeEmStat(bufPtr, fi)
	return 0
}
func (s *Shim) X__syscall_fstat64(fd, bufPtr int32) int32 {
	// musl-level fds only exist for stdio here (no openat); nothing to stat.
	s.logf("STUB __syscall_fstat64 -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_newfstatat(dirFd, pathPtr, bufPtr, flags int32) int32 {
	s.logf("STUB __syscall_newfstatat -> -ENOSYS")
	return -wasiENOSYS
}

// __syscall_getcwd(buf, size): writes the host working directory NUL-terminated
// into buf and returns the byte count INCLUDING the NUL (emscripten's
// library_syscalls contract; -EINVAL for size==0, -ERANGE when it won't fit,
// emscripten errno numbering). DuckDB's FileSystem::GetWorkingDirectory ("IO
// Error: Could not get working directory!") and relative-path
// canonicalization sit directly on this.
func (s *Shim) X__syscall_getcwd(bufPtr, size int32) int32 {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return -wasiENOSYS
	}
	if size == 0 {
		return -28 // -EINVAL (emscripten numbering)
	}
	need := int32(len(cwd) + 1)
	mem := s.memb()
	if need > size || int(bufPtr)+int(need) > len(mem) {
		return -68 // -ERANGE
	}
	copy(mem[bufPtr:], cwd)
	mem[bufPtr+need-1] = 0
	return need
}
func (s *Shim) X__syscall_unlinkat(dirFd, pathPtr, flags int32) int32 {
	s.logf("STUB __syscall_unlinkat -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_rmdir(pathPtr int32) int32 {
	s.logf("STUB __syscall_rmdir -> -ENOSYS")
	return -wasiENOSYS
}

// __syscall_mkdirat: emscripten passes AT_FDCWD (-100) for plain mkdir();
// relative paths resolve against the host cwd, matching getcwd above.
func (s *Shim) X__syscall_mkdirat(dirFd, pathPtr, mode int32) int32 {
	const atFdCwd = -100
	path := s.cString(pathPtr)
	if path == "" {
		return -emENOENT
	}
	if dirFd != atFdCwd && !os.IsPathSeparator(path[0]) {
		s.logf("__syscall_mkdirat dirfd=%d unsupported -> -ENOSYS", dirFd)
		return -wasiENOSYS
	}
	if err := os.Mkdir(path, fs.FileMode(uint32(mode)&0o777)); err != nil {
		return -emErrnoOf(err)
	}
	return 0
}

// ---- abort / assert -------------------------------------------------------

// abort(): wasm reached an unrecoverable state. Unwind to the run harness as an
// error rather than killing the test binary.
func (s *Shim) Xabort() {
	s.logf("abort() called")
	panic(ExitError{Code: 134, Reason: "abort"})
}

// __assert_fail(condPtr, filePtr, line, fnPtr): C assert. Read the strings for
// a useful message, then unwind.
func (s *Shim) X__assert_fail(condPtr, filePtr, line, fnPtr int32) {
	msg := fmt.Sprintf("__assert_fail cond@%d file@%d line=%d fn@%d", condPtr, filePtr, line, fnPtr)
	s.logf("%s", msg)
	panic(ExitError{Code: 134, Reason: msg})
}

// _emscripten_runtime_keepalive_clear / _tzset_js / _localtime_js / _mktime_js:
// time helpers some builds import. Provide harmless behavior.
func (s *Shim) X_emscripten_runtime_keepalive_clear() {}
func (s *Shim) X_tzset_js(timezonePtr, daylightPtr, stdNamePtr, dstNamePtr int32) {
	s.logf("STUB _tzset_js (no timezone db)")
}

// ---- residual surface the real DuckDB standalone build imports ---------------
//
// DuckDB's full C-API wasm imports a wider syscall + socket surface than the
// validation poc.wasm did (extra __syscall_*, fd_pread/pwrite, getaddrinfo/
// getnameinfo). For an IN-MEMORY `SELECT 1` none of these are reachable, so they
// are stubbed to fail-and-log: a non-empty Log after a query is the proof that
// the in-memory path stayed clean. The emscripten __syscall_* family returns a
// NEGATIVE errno (musl convention), WASI fd_* returns a positive errno, and the
// getaddrinfo/getnameinfo netdb calls return a positive EAI/error code.

// fd_pread / fd_pwrite (wasi) are implemented in fs.go.

// Additional emscripten __syscall_* (FILESYSTEM=0 + :memory: must not call these).
func (s *Shim) X__syscall_faccessat(dirFd, pathPtr, mode, flags int32) int32 {
	s.logf("STUB __syscall_faccessat -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_ftruncate64(fd int32, length int64) int32 {
	s.logf("STUB __syscall_ftruncate64 -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_getdents64(fd, direntPtr, count int32) int32 {
	s.logf("STUB __syscall_getdents64 -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_readlinkat(dirFd, pathPtr, bufPtr, bufSize int32) int32 {
	s.logf("STUB __syscall_readlinkat -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_renameat(oldDirFd, oldPathPtr, newDirFd, newPathPtr int32) int32 {
	s.logf("STUB __syscall_renameat -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_statfs64(pathPtr, size, bufPtr int32) int32 {
	s.logf("STUB __syscall_statfs64 -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_poll(fdsPtr, nfds, timeout int32) int32 {
	s.logf("STUB __syscall_poll -> -ENOSYS")
	return -wasiENOSYS
}

// Socket __syscall_* family (no networking for an in-memory query).
func (s *Shim) X__syscall_socket(domain, typ, protocol, a3, a4, a5 int32) int32 {
	s.logf("STUB __syscall_socket -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_bind(fd, addrPtr, addrLen, a3, a4, a5 int32) int32 {
	s.logf("STUB __syscall_bind -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_connect(fd, addrPtr, addrLen, a3, a4, a5 int32) int32 {
	s.logf("STUB __syscall_connect -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_getpeername(fd, addrPtr, addrLenPtr, a3, a4, a5 int32) int32 {
	s.logf("STUB __syscall_getpeername -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_getsockname(fd, addrPtr, addrLenPtr, a3, a4, a5 int32) int32 {
	s.logf("STUB __syscall_getsockname -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_getsockopt(fd, level, optname, optvalPtr, optlenPtr, a5 int32) int32 {
	s.logf("STUB __syscall_getsockopt -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_sendto(fd, bufPtr, length, flags, addrPtr, addrLen int32) int32 {
	s.logf("STUB __syscall_sendto -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_recvfrom(fd, bufPtr, length, flags, addrPtr, addrLenPtr int32) int32 {
	s.logf("STUB __syscall_recvfrom -> -ENOSYS")
	return -wasiENOSYS
}

// getaddrinfo / getnameinfo (netdb): EAI_FAIL (positive). Unused for :memory:.
func (s *Shim) Xgetaddrinfo(nodePtr, servicePtr, hintsPtr, resPtr int32) int32 {
	s.logf("STUB getaddrinfo -> EAI_FAIL")
	return 4 // EAI_FAIL
}
func (s *Shim) Xgetnameinfo(addrPtr, addrLen, hostPtr, hostLen, servPtr, servLen, flags int32) int32 {
	s.logf("STUB getnameinfo -> EAI_FAIL")
	return 4 // EAI_FAIL
}
