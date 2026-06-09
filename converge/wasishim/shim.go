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
	"fmt"
	"io"
	"os"
	"sync"
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

// environ_sizes_get(countPtr, bufSizePtr): empty environment.
func (s *Shim) Xenviron_sizes_get(countPtr, bufSizePtr int32) int32 {
	mem := s.memb()
	binary.LittleEndian.PutUint32(mem[countPtr:], 0)
	binary.LittleEndian.PutUint32(mem[bufSizePtr:], 0)
	return wasiESUCCESS
}

// environ_get(environPtr, bufPtr): nothing to write for an empty environment.
func (s *Shim) Xenviron_get(environPtr, bufPtr int32) int32 { return wasiESUCCESS }

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
func (s *Shim) X__syscall_stat64(pathPtr, bufPtr int32) int32 {
	s.logf("STUB __syscall_stat64 -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_lstat64(pathPtr, bufPtr int32) int32 {
	s.logf("STUB __syscall_lstat64 -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_fstat64(fd, bufPtr int32) int32 {
	s.logf("STUB __syscall_fstat64 -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_newfstatat(dirFd, pathPtr, bufPtr, flags int32) int32 {
	s.logf("STUB __syscall_newfstatat -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_getcwd(bufPtr, size int32) int32 {
	s.logf("STUB __syscall_getcwd -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_unlinkat(dirFd, pathPtr, flags int32) int32 {
	s.logf("STUB __syscall_unlinkat -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_rmdir(pathPtr int32) int32 {
	s.logf("STUB __syscall_rmdir -> -ENOSYS")
	return -wasiENOSYS
}
func (s *Shim) X__syscall_mkdirat(dirFd, pathPtr, mode int32) int32 {
	s.logf("STUB __syscall_mkdirat -> -ENOSYS")
	return -wasiENOSYS
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
