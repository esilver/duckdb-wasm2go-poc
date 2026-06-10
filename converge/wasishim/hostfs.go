// hostfs.go implements the Tier-2 "Path B" host filesystem surface: the small
// set of env.host_* functions that the custom DuckDB FileSystem compiled into
// duckdb_fs.wasm (see ../../host_fs.cpp) calls to reach the REAL host disk.
//
// Unlike fs.go (which emulates the full WASI snapshot-preview1 seam), this layer
// is deliberately tiny: the wasm side already translated DuckDB's FileSystem
// virtuals into plain pread/pwrite/size/trunc calls against an integer fd, so
// here we only keep an fd -> *os.File table and forward to the `os` package.
// Pure Go, CGO_ENABLED=0.
//
// ABI (matches the extern "C" decls in host_fs.cpp):
//
//	host_open(path*, pathlen, flags) -> int64 fd (>=0) or -errno
//	host_pread(fd, buf*, n, off)     -> int64 bytes read   or -errno
//	host_pwrite(fd, buf*, n, off)    -> int64 bytes written or -errno
//	host_size(fd)                    -> int64 size or -errno
//	host_close(fd)                   -> int32 0 or -errno
//	host_exists(path*, pathlen)      -> int32 1/0 or -errno
//	host_trunc(fd, n)                -> int32 0 or -errno
//	host_mtime(fd)                   -> int64 unix seconds or -errno
//
// All pointers are byte offsets into the module's linear memory (read via Mem());
// path arguments are length-counted (NOT NUL-terminated) since DuckDB passes
// std::string::c_str()+size().
package wasishim

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// host_open flag bits (must match HOSTO_* in host_fs.cpp).
const (
	hostoRead    = 1 << 0
	hostoWrite   = 1 << 1
	hostoCreate  = 1 << 2
	hostoTrunc   = 1 << 3
	hostoPrivate = 1 << 4 // create 0600 (FILE_FLAGS_PRIVATE: persistent secrets)
)

// errnoOf maps a Go filesystem error to a POSIX errno (positive). Callers negate
// it for the -errno return convention.
func errnoOf(err error) int32 {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return int32(errno)
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return int32(syscall.ENOENT)
	case errors.Is(err, fs.ErrExist):
		return int32(syscall.EEXIST)
	case errors.Is(err, fs.ErrPermission):
		return int32(syscall.EACCES)
	default:
		return int32(syscall.EIO)
	}
}

// hostString reads a length-counted path out of module memory.
func (s *Shim) hostString(ptr, n int32) string {
	if ptr == 0 || n <= 0 {
		return ""
	}
	mem := s.memb()
	end := ptr + n
	if int(end) > len(mem) {
		end = int32(len(mem))
	}
	return string(mem[ptr:end])
}

// hostAllocFd inserts f and returns a fresh descriptor. Caller holds s.mu.
func (s *Shim) hostAllocFd(f *os.File) int32 {
	if s.hostFds == nil {
		s.hostFds = make(map[int32]*os.File)
		s.hostNextFd = 1 // 0 is a valid fd but we start at 1 to keep 0 "falsy-free"
	}
	fd := s.hostNextFd
	s.hostNextFd++
	s.hostFds[fd] = f
	return fd
}

func (s *Shim) hostLookup(fd int32) *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostFds == nil {
		return nil
	}
	return s.hostFds[fd]
}

// Xhost_open opens (or creates) a host file and returns an fd or -errno.
func (s *Shim) Xhost_open(pathPtr, pathLen, flags int32) int64 {
	path := s.hostString(pathPtr, pathLen)
	if path == "" {
		return int64(-int32(syscall.EINVAL))
	}
	var oflag int
	switch {
	case flags&hostoWrite != 0 && flags&hostoRead != 0:
		oflag = os.O_RDWR
	case flags&hostoWrite != 0:
		oflag = os.O_WRONLY
	default:
		oflag = os.O_RDONLY
	}
	if flags&hostoCreate != 0 {
		oflag |= os.O_CREATE
	}
	if flags&hostoTrunc != 0 {
		oflag |= os.O_TRUNC
	}
	perm := os.FileMode(0o644)
	if flags&hostoPrivate != 0 {
		perm = 0o600 // LocalFileSystem::IsPrivateFile (lstat) checks group/other bits
	}
	f, err := os.OpenFile(path, oflag, perm)
	if err != nil {
		return int64(-errnoOf(err))
	}
	s.mu.Lock()
	fd := s.hostAllocFd(f)
	s.mu.Unlock()
	return int64(fd)
}

// Xhost_pread reads n bytes at absolute offset off into module memory at buf.
func (s *Shim) Xhost_pread(fd, bufPtr int32, n, off int64) int64 {
	f := s.hostLookup(fd)
	if f == nil {
		return int64(-int32(syscall.EBADF))
	}
	if n <= 0 {
		return 0
	}
	mem := s.memb()
	if int(bufPtr) < 0 || int64(bufPtr)+n > int64(len(mem)) {
		return int64(-int32(syscall.EFAULT))
	}
	got, err := f.ReadAt(mem[bufPtr:int64(bufPtr)+n], off)
	// ReadAt returns io.EOF when fewer than len bytes remained in the file; that
	// is a normal short read for our pread contract (return bytes actually read).
	// Only a genuine error that produced zero bytes is a failure.
	if err != nil && !errors.Is(err, io.EOF) && got == 0 {
		return int64(-errnoOf(err))
	}
	return int64(got)
}

// Xhost_pwrite writes n bytes from module memory at buf to absolute offset off.
func (s *Shim) Xhost_pwrite(fd, bufPtr int32, n, off int64) int64 {
	f := s.hostLookup(fd)
	if f == nil {
		return int64(-int32(syscall.EBADF))
	}
	if n <= 0 {
		return 0
	}
	mem := s.memb()
	if int(bufPtr) < 0 || int64(bufPtr)+n > int64(len(mem)) {
		return int64(-int32(syscall.EFAULT))
	}
	put, err := f.WriteAt(mem[bufPtr:int64(bufPtr)+n], off)
	if err != nil && put == 0 {
		return int64(-errnoOf(err))
	}
	return int64(put)
}

// Xhost_size returns the current size of fd in bytes, or -errno.
func (s *Shim) Xhost_size(fd int32) int64 {
	f := s.hostLookup(fd)
	if f == nil {
		return int64(-int32(syscall.EBADF))
	}
	fi, err := f.Stat()
	if err != nil {
		return int64(-errnoOf(err))
	}
	return fi.Size()
}

// Xhost_close closes fd. 0 on success or -errno.
func (s *Shim) Xhost_close(fd int32) int32 {
	s.mu.Lock()
	f := s.hostFds[fd]
	if f != nil {
		delete(s.hostFds, fd)
	}
	s.mu.Unlock()
	if f == nil {
		return -int32(syscall.EBADF)
	}
	if err := f.Close(); err != nil {
		return -errnoOf(err)
	}
	return 0
}

// Xhost_exists reports whether path is an existing regular file (1), absent (0),
// or returns -errno on an unexpected stat error.
func (s *Shim) Xhost_exists(pathPtr, pathLen int32) int32 {
	path := s.hostString(pathPtr, pathLen)
	if path == "" {
		return -int32(syscall.EINVAL)
	}
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0
		}
		return -errnoOf(err)
	}
	if fi.IsDir() {
		return 0
	}
	return 1
}

// Xhost_trunc truncates fd to n bytes. 0 on success or -errno.
func (s *Shim) Xhost_trunc(fd int32, n int64) int32 {
	f := s.hostLookup(fd)
	if f == nil {
		return -int32(syscall.EBADF)
	}
	if err := f.Truncate(n); err != nil {
		return -errnoOf(err)
	}
	return 0
}

// Xhost_unlink removes a file by path. 0 on success or -errno.
func (s *Shim) Xhost_unlink(pathPtr, pathLen int32) int32 {
	path := s.hostString(pathPtr, pathLen)
	if path == "" {
		return -int32(syscall.EINVAL)
	}
	if err := os.Remove(path); err != nil {
		return -errnoOf(err)
	}
	return 0
}

// Xhost_rename atomically renames oldp -> newp. 0 on success or -errno.
func (s *Shim) Xhost_rename(oldPtr, oldLen, newPtr, newLen int32) int32 {
	oldp := s.hostString(oldPtr, oldLen)
	newp := s.hostString(newPtr, newLen)
	if oldp == "" || newp == "" {
		return -int32(syscall.EINVAL)
	}
	if err := os.Rename(oldp, newp); err != nil {
		return -errnoOf(err)
	}
	return 0
}

// Xhost_isdir reports whether path is a directory (1), not a dir (0), or -errno.
func (s *Shim) Xhost_isdir(pathPtr, pathLen int32) int32 {
	path := s.hostString(pathPtr, pathLen)
	if path == "" {
		return -int32(syscall.EINVAL)
	}
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0
		}
		return -errnoOf(err)
	}
	if fi.IsDir() {
		return 1
	}
	return 0
}

// Xhost_sync flushes fd to stable storage. 0 on success or -errno.
func (s *Shim) Xhost_sync(fd int32) int32 {
	f := s.hostLookup(fd)
	if f == nil {
		return -int32(syscall.EBADF)
	}
	if err := f.Sync(); err != nil {
		return -errnoOf(err)
	}
	return 0
}

// Xhost_mtime returns fd's last-modification time in whole unix seconds, -errno
// on error.
func (s *Shim) Xhost_mtime(fd int32) int64 {
	f := s.hostLookup(fd)
	if f == nil {
		return int64(-int32(syscall.EBADF))
	}
	fi, err := f.Stat()
	if err != nil {
		return int64(-errnoOf(err))
	}
	return fi.ModTime().Unix()
}

// Xhost_mkdir creates path (with parents). 0 on success or -errno.
func (s *Shim) Xhost_mkdir(pathPtr, pathLen int32) int32 {
	path := s.hostString(pathPtr, pathLen)
	if path == "" {
		return -int32(syscall.EINVAL)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return -errnoOf(err)
	}
	return 0
}

// Xhost_rmdir removes the directory tree at path (recursive — DuckDB's
// RemoveDirectory is used on its temp dir, which may still hold spill files).
// 0 on success or -errno.
func (s *Shim) Xhost_rmdir(pathPtr, pathLen int32) int32 {
	path := s.hostString(pathPtr, pathLen)
	if path == "" {
		return -int32(syscall.EINVAL)
	}
	if err := os.RemoveAll(path); err != nil {
		return -errnoOf(err)
	}
	return 0
}

// Xhost_listdir writes path's entries into module memory at out as
// newline-terminated names (directories suffixed '/'). Returns bytes written
// or -errno (-ERANGE if the listing does not fit outcap).
func (s *Shim) Xhost_listdir(pathPtr, pathLen, out, outcap int32) int32 {
	path := s.hostString(pathPtr, pathLen)
	if path == "" {
		return -int32(syscall.EINVAL)
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return -errnoOf(err)
	}
	mem := s.memb()
	n := int32(0)
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		need := int32(len(name)) + 1
		if n+need > outcap || int(out+n+need) > len(mem) {
			return -int32(syscall.ERANGE)
		}
		copy(mem[out+n:], name)
		mem[out+n+int32(len(name))] = '\n'
		n += need
	}
	return n
}
