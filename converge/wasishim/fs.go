// fs.go implements the WASI snapshot preview1 FILESYSTEM surface for the shim,
// backed by the real host OS via the standard `os` package (pure Go, no CGO).
// It lets a wasm2go-transpiled DuckDB open a FILE-BACKED database and read
// Parquet/CSV/JSON files off disk.
//
// Design
//
//	fd table:   map[int32]*fdEntry, descriptors start at 3 (0/1/2 are reserved
//	            for stdin/stdout/stderr, handled in shim.go). fd 3 is the single
//	            preopened directory ("/" by default, see SetPreopen) so the wasm
//	            can resolve absolute paths under it.
//	preopen:    one directory, advertised via fd_prestat_get / fd_prestat_dir_name.
//	            All WASI paths are resolved (lexically, then joined) under the
//	            preopen's host root, so the wasm cannot escape it via "..".
//	resolution: path_open and the path_* calls take a dirfd; we resolve the WASI
//	            path against that fd's host directory (or the preopen root).
//
// All methods are `X<wasiname>` on *Shim, return an int32 WASI errno (0 == ok),
// take i32 args that are byte offsets into Mem() (i64 where the spec says so),
// and use little-endian for every out-param integer written back to memory.
//
// Hot path for a DuckDB file query: path_open, fd_read / fd_pread, fd_seek,
// fd_filestat_get, fd_fdstat_get, fd_close. Those are implemented carefully;
// the rare ops (rename, mkdir, readdir, set_times) are implemented too but are
// not on the hot path.
package wasishim

import (
	"encoding/binary"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ---- WASI errno (subset we map Go errors to) ------------------------------

const (
	wasiEACCES       = 2
	wasiEEXIST       = 20
	wasiEINVAL       = 28
	wasiEIO          = 29
	wasiEISDIR       = 31
	wasiENOENT       = 44
	wasiENOTDIR      = 54
	wasiENOTEMPTY    = 55
	wasiEOVERFLOW    = 61
	wasiEPERM        = 63 // also used for "not permitted"
	wasiENOTCAPABLE  = 76
)

// ---- WASI filetype --------------------------------------------------------

const (
	filetypeUnknown      = 0
	filetypeBlockDevice  = 1
	filetypeCharDevice   = 2
	filetypeDirectory    = 3
	filetypeRegularFile  = 4
	filetypeSocketDgram  = 5
	filetypeSocketStream = 6
	filetypeSymbolicLink = 7
)

// ---- WASI oflags (path_open) ----------------------------------------------

const (
	oflagCreat     = 1 << 0 // create if nonexistent
	oflagDirectory = 1 << 1 // fail if not a directory
	oflagExcl      = 1 << 2 // fail if already exists
	oflagTrunc     = 1 << 3 // truncate to zero size
)

// ---- WASI fdflags ---------------------------------------------------------

const (
	fdflagAppend   = 1 << 0
	fdflagDSync    = 1 << 1
	fdflagNonblock = 1 << 2
	fdflagRSync    = 1 << 3
	fdflagSync     = 1 << 4
)

// ---- WASI rights (advertised generously for files & the preopen dir) ------

const (
	rightFdDatasync         = 1 << 0
	rightFdRead             = 1 << 1
	rightFdSeek             = 1 << 2
	rightFdFdstatSetFlags   = 1 << 3
	rightFdSync             = 1 << 4
	rightFdTell             = 1 << 5
	rightFdWrite            = 1 << 6
	rightFdAdvise           = 1 << 7
	rightFdAllocate         = 1 << 8
	rightPathCreateDir      = 1 << 9
	rightPathCreateFile     = 1 << 10
	rightPathLinkSource     = 1 << 11
	rightPathLinkTarget     = 1 << 12
	rightPathOpen           = 1 << 13
	rightFdReaddir          = 1 << 14
	rightPathReadlink       = 1 << 15
	rightPathRenameSource   = 1 << 16
	rightPathRenameTarget   = 1 << 17
	rightPathFilestatGet    = 1 << 18
	rightPathFilestatSetSz  = 1 << 19
	rightPathFilestatSetTim = 1 << 20
	rightFdFilestatGet      = 1 << 21
	rightFdFilestatSetSize  = 1 << 22
	rightFdFilestatSetTimes = 1 << 23
	rightPathRemoveDir      = 1 << 27
	rightPathUnlinkFile     = 1 << 28
)

// fileRights / dirRights are the rights we report for an open regular file and
// for a directory (including the preopen). DuckDB only inspects these loosely.
const (
	fileRights = rightFdDatasync | rightFdRead | rightFdSeek | rightFdFdstatSetFlags |
		rightFdSync | rightFdTell | rightFdWrite | rightFdAdvise | rightFdAllocate |
		rightFdFilestatGet | rightFdFilestatSetSize | rightFdFilestatSetTimes

	dirRights = rightPathCreateDir | rightPathCreateFile | rightPathLinkSource |
		rightPathLinkTarget | rightPathOpen | rightFdReaddir | rightPathReadlink |
		rightPathRenameSource | rightPathRenameTarget | rightPathFilestatGet |
		rightPathFilestatSetSz | rightPathFilestatSetTim | rightPathRemoveDir |
		rightPathUnlinkFile | rightFdFilestatGet | fileRights
)

// preopenFd is the fixed descriptor for the single preopened directory.
const preopenFd = 3

// fdEntry is one row of the fd table.
type fdEntry struct {
	file      *os.File // the live OS handle (also used for directories)
	hostPath  string   // absolute host path
	isPreopen bool     // true for the preopen dir at fd 3
	isDir     bool
	appendF   bool // open in append mode (fd_write seeks to end)
	// readdir cursor cache: directory entries, lazily loaded by fd_readdir.
	dirEntries []os.DirEntry
	dirLoaded  bool
}

// SetPreopen configures the host directory exposed as the wasm preopen (fd 3)
// and the WASI name the wasm sees for it (e.g. "/"). The integrator should call
// this once before running the module; if never called, the FS lazily defaults
// to root "/" mapped to host "/". Safe to call multiple times before use.
func (s *Shim) SetPreopen(wasiName, hostRoot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wasiName == "" {
		wasiName = "/"
	}
	if hostRoot == "" {
		hostRoot = "/"
	}
	if abs, err := filepath.Abs(hostRoot); err == nil {
		hostRoot = abs
	}
	s.preopenName = wasiName
	s.preopenRoot = hostRoot
	// Reset the table so the new preopen takes effect.
	s.fds = nil
}

// fsEnsure lazily initializes the fd table + preopen on first filesystem use.
// Caller must hold s.mu.
func (s *Shim) fsEnsure() {
	if s.fds != nil {
		return
	}
	if s.preopenName == "" {
		s.preopenName = "/"
	}
	if s.preopenRoot == "" {
		s.preopenRoot = "/"
	}
	s.fds = map[int32]*fdEntry{
		preopenFd: {hostPath: s.preopenRoot, isPreopen: true, isDir: true},
	}
	s.nextFd = preopenFd + 1
}

// lookup returns the fdEntry for fd, or nil. Caller must hold s.mu.
func (s *Shim) lookup(fd int32) *fdEntry {
	s.fsEnsure()
	return s.fds[fd]
}

// allocFd inserts e and returns its new descriptor. Caller must hold s.mu.
func (s *Shim) allocFd(e *fdEntry) int32 {
	s.fsEnsure()
	fd := s.nextFd
	s.nextFd++
	s.fds[fd] = e
	return fd
}

// ---- error mapping --------------------------------------------------------

// errno maps a Go error (from the os/syscall packages) to a WASI errno.
func errno(err error) int32 {
	if err == nil {
		return wasiESUCCESS
	}
	switch {
	case os.IsNotExist(err):
		return wasiENOENT
	case os.IsExist(err):
		return wasiEEXIST
	case os.IsPermission(err):
		return wasiEACCES
	}
	var e syscall.Errno
	if asErrno(err, &e) {
		switch e {
		case syscall.ENOENT:
			return wasiENOENT
		case syscall.EEXIST:
			return wasiEEXIST
		case syscall.EACCES:
			return wasiEACCES
		case syscall.EPERM:
			return wasiEPERM
		case syscall.EISDIR:
			return wasiEISDIR
		case syscall.ENOTDIR:
			return wasiENOTDIR
		case syscall.ENOTEMPTY:
			return wasiENOTEMPTY
		case syscall.EINVAL:
			return wasiEINVAL
		case syscall.EIO:
			return wasiEIO
		}
	}
	return wasiEIO
}

// asErrno unwraps err to a syscall.Errno if possible.
func asErrno(err error, out *syscall.Errno) bool {
	for err != nil {
		if e, ok := err.(syscall.Errno); ok {
			*out = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		// os.PathError / os.LinkError / os.SyscallError carry an Err field
		// reachable via Unwrap already; nothing else to do.
		break
	}
	return false
}

// ---- memory helpers -------------------------------------------------------

// readStr reads a path string of pathLen bytes at ptr from wasm memory.
func (s *Shim) readStr(ptr, n int32) string {
	if ptr <= 0 || n <= 0 {
		return ""
	}
	mem := s.memb()
	if int(ptr)+int(n) > len(mem) {
		n = int32(len(mem)) - ptr
		if n <= 0 {
			return ""
		}
	}
	return string(mem[ptr : ptr+n])
}

func (s *Shim) putU32(ptr int32, v uint32) {
	if ptr != 0 {
		binary.LittleEndian.PutUint32(s.memb()[ptr:], v)
	}
}

func (s *Shim) putU64(ptr int32, v uint64) {
	if ptr != 0 {
		binary.LittleEndian.PutUint64(s.memb()[ptr:], v)
	}
}

// ---- path resolution ------------------------------------------------------

// resolvePath joins a WASI path (relative to dirfd's host directory, or to the
// preopen root for absolute paths) into a confined absolute host path. It
// rejects escapes out of the preopen root. Returns ("", false) if dirfd is bad
// or the path escapes the sandbox.
func (s *Shim) resolvePath(dirfd int32, wasiPath string) (string, bool) {
	base := s.lookup(dirfd)
	if base == nil || !base.isDir {
		return "", false
	}
	// Clean the WASI path lexically. A leading "/" is treated as relative to
	// the preopen root (the wasm's filesystem root), matching how DuckDB and
	// WASI libpreopen present absolute paths.
	clean := path.Clean("/" + strings.ReplaceAll(wasiPath, "\\", "/"))
	// clean now starts with "/" and has no "." or ".." segments that escape.
	rel := strings.TrimPrefix(clean, "/")

	var host string
	if strings.HasPrefix(wasiPath, "/") {
		// Absolute: anchor at the preopen root.
		host = filepath.Join(s.preopenRoot, filepath.FromSlash(rel))
	} else {
		host = filepath.Join(base.hostPath, filepath.FromSlash(rel))
	}
	// Confinement: the resolved host path must stay within the preopen root.
	root := filepath.Clean(s.preopenRoot)
	hc := filepath.Clean(host)
	if root != string(filepath.Separator) { // "/" contains everything
		if hc != root && !strings.HasPrefix(hc, root+string(filepath.Separator)) {
			return "", false
		}
	}
	return hc, true
}

// ---- filetype helpers -----------------------------------------------------

func modeToFiletype(m os.FileMode) byte {
	switch {
	case m&os.ModeDir != 0:
		return filetypeDirectory
	case m&os.ModeSymlink != 0:
		return filetypeSymbolicLink
	case m&os.ModeDevice != 0:
		if m&os.ModeCharDevice != 0 {
			return filetypeCharDevice
		}
		return filetypeBlockDevice
	case m&os.ModeSocket != 0:
		return filetypeSocketStream
	case m.IsRegular():
		return filetypeRegularFile
	default:
		return filetypeUnknown
	}
}

// ---- iovec scatter/gather -------------------------------------------------

// writeIovecs reads len(data)-worth of bytes into the iovec buffers described
// in memory (each iovec = {buf u32, len u32}), copying up to read 'r' provides.
// It is used by fd_read/fd_pread. Returns total bytes read and a WASI errno.
func (s *Shim) readInto(r io.Reader, iovsPtr, iovsLen int32) (uint32, int32) {
	mem := s.memb()
	var total uint32
	for i := int32(0); i < iovsLen; i++ {
		base := iovsPtr + i*8
		if int(base)+8 > len(mem) {
			return total, wasiEINVAL
		}
		ptr := int32(binary.LittleEndian.Uint32(mem[base:]))
		ln := int32(binary.LittleEndian.Uint32(mem[base+4:]))
		if ln <= 0 {
			continue
		}
		if int(ptr)+int(ln) > len(mem) {
			return total, wasiEINVAL
		}
		n, err := r.Read(mem[ptr : ptr+ln])
		total += uint32(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, errno(err)
		}
		if n < int(ln) {
			// Short read: the file had fewer bytes than the buffer; stop here
			// rather than spinning (a regular file read returns what it has).
			break
		}
	}
	return total, wasiESUCCESS
}

// writeFrom gather-writes the iovec buffers to w. Used by fd_write/fd_pwrite.
func (s *Shim) writeFrom(w io.Writer, iovsPtr, iovsLen int32) (uint32, int32) {
	mem := s.memb()
	var total uint32
	for i := int32(0); i < iovsLen; i++ {
		base := iovsPtr + i*8
		if int(base)+8 > len(mem) {
			return total, wasiEINVAL
		}
		ptr := int32(binary.LittleEndian.Uint32(mem[base:]))
		ln := int32(binary.LittleEndian.Uint32(mem[base+4:]))
		if ln <= 0 {
			continue
		}
		if int(ptr)+int(ln) > len(mem) {
			return total, wasiEINVAL
		}
		n, err := w.Write(mem[ptr : ptr+ln])
		total += uint32(n)
		if err != nil {
			return total, errno(err)
		}
	}
	return total, wasiESUCCESS
}

// ============================================================================
// preopen
// ============================================================================

// fd_prestat_get(fd, buf): report the preopen. prestat layout (8 bytes):
//
//	+0 u8   tag  (0 == preopen dir)
//	+4 u32  pr_name_len (length of the dir name, NOT NUL-terminated)
func (s *Shim) Xfd_prestat_get(fd, buf int32) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.lookup(fd)
	if e == nil || !e.isPreopen {
		// EBADF ends the wasm's preopen scan cleanly (it walks fds upward until
		// it gets EBADF).
		return wasiEBADF
	}
	mem := s.memb()
	mem[buf] = 0 // tag: preopendir
	mem[buf+1] = 0
	mem[buf+2] = 0
	mem[buf+3] = 0
	binary.LittleEndian.PutUint32(mem[buf+4:], uint32(len(s.preopenName)))
	return wasiESUCCESS
}

// fd_prestat_dir_name(fd, path, path_len): write the preopen's name.
func (s *Shim) Xfd_prestat_dir_name(fd, pathPtr, pathLen int32) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.lookup(fd)
	if e == nil || !e.isPreopen {
		return wasiEBADF
	}
	name := s.preopenName
	if int(pathLen) < len(name) {
		return wasiENAMETOOLONG
	}
	mem := s.memb()
	if int(pathPtr)+len(name) > len(mem) {
		return wasiEINVAL
	}
	copy(mem[pathPtr:pathPtr+int32(len(name))], name)
	return wasiESUCCESS
}

const wasiENAMETOOLONG = 37

// ============================================================================
// path_open
// ============================================================================

// path_open opens (and optionally creates) a file under dirfd and writes the
// new descriptor to *opened_fd.
func (s *Shim) Xpath_open(dirFd, dirFlags, pathPtr, pathLen, oflags int32, fsRightsBase, fsRightsInheriting int64, fdFlags, openedFdPtr int32) int32 {
	name := s.readStr(pathPtr, pathLen)
	s.mu.Lock()
	defer s.mu.Unlock()

	host, ok := s.resolvePath(dirFd, name)
	if !ok {
		return wasiENOTCAPABLE
	}

	// Translate WASI oflags + rights into os open flags.
	flag := os.O_RDONLY
	wantWrite := fsRightsBase&(rightFdWrite|rightFdFilestatSetSize|rightFdAllocate) != 0
	if wantWrite {
		flag = os.O_RDWR
	}
	if oflags&oflagCreat != 0 {
		flag |= os.O_CREATE
		if flag&os.O_RDWR == 0 {
			flag |= os.O_RDWR // creating implies we can write
		}
	}
	if oflags&oflagExcl != 0 {
		flag |= os.O_EXCL
	}
	if oflags&oflagTrunc != 0 {
		flag |= os.O_TRUNC
		if flag&(os.O_WRONLY|os.O_RDWR) == 0 {
			flag |= os.O_RDWR
		}
	}
	appendMode := fdFlags&fdflagAppend != 0
	if appendMode {
		flag |= os.O_APPEND
		if flag&(os.O_WRONLY|os.O_RDWR) == 0 {
			flag |= os.O_RDWR
		}
	}

	// O_DIRECTORY: require a directory. We open it read-only as a handle.
	if oflags&oflagDirectory != 0 {
		fi, err := os.Stat(host)
		if err != nil {
			return errno(err)
		}
		if !fi.IsDir() {
			return wasiENOTDIR
		}
		f, err := os.Open(host)
		if err != nil {
			return errno(err)
		}
		fd := s.allocFd(&fdEntry{file: f, hostPath: host, isDir: true})
		s.putU32(openedFdPtr, uint32(fd))
		return wasiESUCCESS
	}

	f, err := os.OpenFile(host, flag, 0o644)
	if err != nil {
		// Opening a directory with O_RDWR fails on some platforms; retry RDONLY
		// and treat as a directory handle if it is one.
		if fi, serr := os.Stat(host); serr == nil && fi.IsDir() {
			df, derr := os.Open(host)
			if derr != nil {
				return errno(derr)
			}
			fd := s.allocFd(&fdEntry{file: df, hostPath: host, isDir: true})
			s.putU32(openedFdPtr, uint32(fd))
			return wasiESUCCESS
		}
		return errno(err)
	}

	fi, err := f.Stat()
	isDir := err == nil && fi.IsDir()
	fd := s.allocFd(&fdEntry{file: f, hostPath: host, isDir: isDir, appendF: appendMode})
	s.putU32(openedFdPtr, uint32(fd))
	return wasiESUCCESS
}

// ============================================================================
// fd_read / fd_pread / fd_write / fd_pwrite
// ============================================================================

// fd_read(fd, iovs, iovs_len, nread): scatter read from the file at its current
// offset.
func (s *Shim) Xfd_read(fd, iovsPtr, iovsLen, nreadPtr int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()
	if e == nil || e.file == nil {
		// fd 0 (stdin): report EOF.
		if fd == 0 {
			s.putU32(nreadPtr, 0)
			return wasiESUCCESS
		}
		return wasiEBADF
	}
	if e.isDir {
		return wasiEISDIR
	}
	n, errc := s.readInto(e.file, iovsPtr, iovsLen)
	s.putU32(nreadPtr, n)
	return errc
}

// fd_pread(fd, iovs, iovs_len, offset, nread): positional read; does not move
// the file offset.
func (s *Shim) Xfd_pread(fd, iovsPtr, iovsLen int32, offset int64, nreadPtr int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()
	if e == nil || e.file == nil {
		return wasiEBADF
	}
	if e.isDir {
		return wasiEISDIR
	}
	r := io.NewSectionReader(e.file, offset, 1<<62)
	n, errc := s.readInto(r, iovsPtr, iovsLen)
	s.putU32(nreadPtr, n)
	return errc
}

// fd_write(fd, iovs, iovs_len, nwritten): gather write. fd 1/2 keep going to
// the host stdout/stderr (delegated to the implementation in shim.go).
func (s *Shim) Xfd_write(fd, iovsPtr, iovsLen, nwrittenPtr int32) int32 {
	if fd == 1 || fd == 2 {
		return s.fdWriteStd(fd, iovsPtr, iovsLen, nwrittenPtr)
	}
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()
	if e == nil || e.file == nil {
		return wasiEBADF
	}
	if e.isDir {
		return wasiEISDIR
	}
	if e.appendF {
		// O_APPEND was set at open: the OS handles end-positioning on write.
	}
	n, errc := s.writeFrom(e.file, iovsPtr, iovsLen)
	s.putU32(nwrittenPtr, n)
	return errc
}

// fdWriteStd handles stdout/stderr, preserving the original shim behavior.
func (s *Shim) fdWriteStd(fd, iovsPtr, iovsLen, nwrittenPtr int32) int32 {
	var w io.Writer
	switch fd {
	case 1:
		w = s.Stdout
	case 2:
		w = s.Stderr
	}
	if w == nil {
		w = io.Discard
	}
	n, errc := s.writeFrom(w, iovsPtr, iovsLen)
	s.putU32(nwrittenPtr, n)
	return errc
}

// fd_pwrite(fd, iovs, iovs_len, offset, nwritten): positional write.
func (s *Shim) Xfd_pwrite(fd, iovsPtr, iovsLen int32, offset int64, nwrittenPtr int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()
	if e == nil || e.file == nil {
		return wasiEBADF
	}
	if e.isDir {
		return wasiEISDIR
	}
	// Gather the iovecs into one buffer, then WriteAt.
	mem := s.memb()
	var buf []byte
	for i := int32(0); i < iovsLen; i++ {
		base := iovsPtr + i*8
		if int(base)+8 > len(mem) {
			return wasiEINVAL
		}
		ptr := int32(binary.LittleEndian.Uint32(mem[base:]))
		ln := int32(binary.LittleEndian.Uint32(mem[base+4:]))
		if ln <= 0 {
			continue
		}
		if int(ptr)+int(ln) > len(mem) {
			return wasiEINVAL
		}
		buf = append(buf, mem[ptr:ptr+ln]...)
	}
	n, err := e.file.WriteAt(buf, offset)
	s.putU32(nwrittenPtr, uint32(n))
	if err != nil {
		return errno(err)
	}
	return wasiESUCCESS
}

// ============================================================================
// fd_seek / fd_tell
// ============================================================================

// fd_seek(fd, offset i64, whence, newoffset): seek and write the new position
// to *newoffset (u64). WASI whence: 0=SET, 1=CUR, 2=END (matches os.SEEK_*).
func (s *Shim) Xfd_seek(fd int32, offset int64, whence, newOffsetPtr int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()
	if e == nil || e.file == nil {
		return wasiEBADF
	}
	var w int
	switch whence {
	case 0:
		w = io.SeekStart
	case 1:
		w = io.SeekCurrent
	case 2:
		w = io.SeekEnd
	default:
		return wasiEINVAL
	}
	pos, err := e.file.Seek(offset, w)
	if err != nil {
		return errno(err)
	}
	s.putU64(newOffsetPtr, uint64(pos))
	return wasiESUCCESS
}

// fd_tell(fd, offset): write the current file offset to *offset (u64).
func (s *Shim) Xfd_tell(fd, offsetPtr int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()
	if e == nil || e.file == nil {
		return wasiEBADF
	}
	pos, err := e.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return errno(err)
	}
	s.putU64(offsetPtr, uint64(pos))
	return wasiESUCCESS
}

// ============================================================================
// fd_close / fd_sync / fd_datasync
// ============================================================================

func (s *Shim) Xfd_close(fd int32) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.lookup(fd)
	if e == nil {
		return wasiEBADF
	}
	if e.isPreopen {
		// Closing the preopen is allowed but pointless; drop it from the table.
		delete(s.fds, fd)
		return wasiESUCCESS
	}
	var err error
	if e.file != nil {
		err = e.file.Close()
	}
	delete(s.fds, fd)
	return errno(err)
}

func (s *Shim) Xfd_sync(fd int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()
	if e == nil || e.file == nil {
		return wasiEBADF
	}
	if e.isDir {
		return wasiESUCCESS
	}
	return errno(e.file.Sync())
}

// fd_datasync: we have no datasync-only primitive in os; Sync is a superset.
func (s *Shim) Xfd_datasync(fd int32) int32 {
	return s.Xfd_sync(fd)
}

// ============================================================================
// fd_fdstat_get / fd_fdstat_set_flags
// ============================================================================

// fdstat layout (24 bytes):
//
//	+0  u8   fs_filetype
//	+2  u16  fs_flags (fdflags)
//	+8  u64  fs_rights_base
//	+16 u64  fs_rights_inheriting
func (s *Shim) Xfd_fdstat_get(fd, buf int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()

	mem := s.memb()
	// Zero the 24-byte struct first.
	for i := int32(0); i < 24; i++ {
		mem[buf+i] = 0
	}

	if e == nil {
		// stdio fds (0,1,2): present as character devices so isatty-like probes
		// behave, with read/write rights.
		switch fd {
		case 0, 1, 2:
			mem[buf] = filetypeCharDevice
			binary.LittleEndian.PutUint64(mem[buf+8:], rightFdRead|rightFdWrite)
			return wasiESUCCESS
		}
		return wasiEBADF
	}

	var ft byte = filetypeRegularFile
	var rights uint64 = fileRights
	if e.isDir {
		ft = filetypeDirectory
		rights = dirRights
	}
	mem[buf] = ft
	var flags uint16
	if e.appendF {
		flags |= fdflagAppend
	}
	binary.LittleEndian.PutUint16(mem[buf+2:], flags)
	binary.LittleEndian.PutUint64(mem[buf+8:], rights)
	binary.LittleEndian.PutUint64(mem[buf+16:], dirRights) // inheriting
	return wasiESUCCESS
}

// fd_fdstat_set_flags: we only track the append flag meaningfully.
func (s *Shim) Xfd_fdstat_set_flags(fd, flags int32) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.lookup(fd)
	if e == nil {
		return wasiEBADF
	}
	e.appendF = flags&fdflagAppend != 0
	return wasiESUCCESS
}

// ============================================================================
// fd_filestat_get / path_filestat_get
// ============================================================================

// filestat layout (64 bytes):
//
//	+0  u64 dev
//	+8  u64 ino
//	+16 u8  filetype  (+7 pad)
//	+24 u64 nlink
//	+32 u64 size
//	+40 u64 atim (ns)
//	+48 u64 mtim (ns)
//	+56 u64 ctim (ns)
func (s *Shim) writeFilestat(buf int32, fi os.FileInfo) {
	mem := s.memb()
	for i := int32(0); i < 64; i++ {
		mem[buf+i] = 0
	}
	dev, ino, nlink := statIdentity(fi)
	binary.LittleEndian.PutUint64(mem[buf:], dev)
	binary.LittleEndian.PutUint64(mem[buf+8:], ino)
	mem[buf+16] = modeToFiletype(fi.Mode())
	binary.LittleEndian.PutUint64(mem[buf+24:], nlink)
	binary.LittleEndian.PutUint64(mem[buf+32:], uint64(fi.Size()))
	mt := uint64(fi.ModTime().UnixNano())
	binary.LittleEndian.PutUint64(mem[buf+40:], mt) // atim (best-effort = mtime)
	binary.LittleEndian.PutUint64(mem[buf+48:], mt) // mtim
	binary.LittleEndian.PutUint64(mem[buf+56:], mt) // ctim
}

func (s *Shim) Xfd_filestat_get(fd, buf int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	s.mu.Unlock()
	if e == nil || e.file == nil {
		return wasiEBADF
	}
	fi, err := e.file.Stat()
	if err != nil {
		return errno(err)
	}
	s.writeFilestat(buf, fi)
	return wasiESUCCESS
}

// path_filestat_get(dirfd, flags, path, path_len, buf). flags bit0 =
// SYMLINK_FOLLOW; we follow by default (os.Stat) and use Lstat when not set.
func (s *Shim) Xpath_filestat_get(dirFd, flags, pathPtr, pathLen, buf int32) int32 {
	name := s.readStr(pathPtr, pathLen)
	s.mu.Lock()
	host, ok := s.resolvePath(dirFd, name)
	s.mu.Unlock()
	if !ok {
		return wasiENOTCAPABLE
	}
	var fi os.FileInfo
	var err error
	if flags&1 != 0 {
		fi, err = os.Stat(host)
	} else {
		fi, err = os.Lstat(host)
	}
	if err != nil {
		return errno(err)
	}
	s.writeFilestat(buf, fi)
	return wasiESUCCESS
}

// ============================================================================
// path_create_directory / unlink / remove_directory / rename / set_times
// ============================================================================

func (s *Shim) Xpath_create_directory(dirFd, pathPtr, pathLen int32) int32 {
	name := s.readStr(pathPtr, pathLen)
	s.mu.Lock()
	host, ok := s.resolvePath(dirFd, name)
	s.mu.Unlock()
	if !ok {
		return wasiENOTCAPABLE
	}
	return errno(os.Mkdir(host, 0o755))
}

func (s *Shim) Xpath_unlink_file(dirFd, pathPtr, pathLen int32) int32 {
	name := s.readStr(pathPtr, pathLen)
	s.mu.Lock()
	host, ok := s.resolvePath(dirFd, name)
	s.mu.Unlock()
	if !ok {
		return wasiENOTCAPABLE
	}
	fi, err := os.Lstat(host)
	if err != nil {
		return errno(err)
	}
	if fi.IsDir() {
		return wasiEISDIR
	}
	return errno(os.Remove(host))
}

func (s *Shim) Xpath_remove_directory(dirFd, pathPtr, pathLen int32) int32 {
	name := s.readStr(pathPtr, pathLen)
	s.mu.Lock()
	host, ok := s.resolvePath(dirFd, name)
	s.mu.Unlock()
	if !ok {
		return wasiENOTCAPABLE
	}
	fi, err := os.Lstat(host)
	if err != nil {
		return errno(err)
	}
	if !fi.IsDir() {
		return wasiENOTDIR
	}
	return errno(os.Remove(host))
}

func (s *Shim) Xpath_rename(oldDirFd, oldPathPtr, oldPathLen, newDirFd, newPathPtr, newPathLen int32) int32 {
	oldName := s.readStr(oldPathPtr, oldPathLen)
	newName := s.readStr(newPathPtr, newPathLen)
	s.mu.Lock()
	oldHost, ok1 := s.resolvePath(oldDirFd, oldName)
	newHost, ok2 := s.resolvePath(newDirFd, newName)
	s.mu.Unlock()
	if !ok1 || !ok2 {
		return wasiENOTCAPABLE
	}
	return errno(os.Rename(oldHost, newHost))
}

// path_filestat_set_times(dirfd, flags, path, path_len, atim, mtim, fst_flags).
// fst_flags: bit0 ATIM, bit1 ATIM_NOW, bit2 MTIM, bit3 MTIM_NOW.
func (s *Shim) Xpath_filestat_set_times(dirFd, flags, pathPtr, pathLen int32, atim, mtim int64, fstFlags int32) int32 {
	name := s.readStr(pathPtr, pathLen)
	s.mu.Lock()
	host, ok := s.resolvePath(dirFd, name)
	s.mu.Unlock()
	if !ok {
		return wasiENOTCAPABLE
	}
	now := time.Now()
	at := now
	mt := now
	if fstFlags&0x1 != 0 { // ATIM
		at = time.Unix(0, atim)
	} else if fstFlags&0x2 == 0 { // neither ATIM nor ATIM_NOW: keep current
		if fi, err := os.Stat(host); err == nil {
			at = fi.ModTime()
		}
	}
	if fstFlags&0x4 != 0 { // MTIM
		mt = time.Unix(0, mtim)
	} else if fstFlags&0x8 == 0 { // neither MTIM nor MTIM_NOW: keep current
		if fi, err := os.Stat(host); err == nil {
			mt = fi.ModTime()
		}
	}
	return errno(os.Chtimes(host, at, mt))
}

// ============================================================================
// fd_readdir
// ============================================================================

// fd_readdir(fd, buf, buf_len, cookie i64, bufused). Serializes directory
// entries into buf as a sequence of (dirent header + name) records. cookie is
// the index of the first entry to emit; we also emit "." and ".." first.
//
// dirent header layout (24 bytes):
//
//	+0  u64 d_next  (cookie of the NEXT entry)
//	+8  u64 d_ino
//	+16 u32 d_namlen
//	+20 u8  d_type  (+3 pad)
func (s *Shim) Xfd_readdir(fd, buf, bufLen int32, cookie int64, bufUsedPtr int32) int32 {
	s.mu.Lock()
	e := s.lookup(fd)
	if e == nil || !e.isDir {
		s.mu.Unlock()
		if e == nil {
			return wasiEBADF
		}
		return wasiENOTDIR
	}
	if !e.dirLoaded {
		ents, err := os.ReadDir(e.hostPath)
		if err != nil {
			s.mu.Unlock()
			return errno(err)
		}
		e.dirEntries = ents
		e.dirLoaded = true
	}
	entries := e.dirEntries
	hostPath := e.hostPath
	s.mu.Unlock()

	mem := s.memb()
	var used int32

	// Build the virtual list: "." (cookie 0), ".." (cookie 1), then real
	// entries at cookie 2+.
	type vent struct {
		name string
		ino  uint64
		ft   byte
	}
	emit := func(idx int64, v vent) bool {
		const hdr = 24
		nameLen := int32(len(v.name))
		// Header.
		if used+hdr > bufLen {
			// Partial header still must be copied (truncated) per spec; but to
			// stay simple and safe, stop: a too-small buffer means the caller
			// will call again with a bigger one.
			room := bufLen - used
			if room > 0 {
				var h [hdr]byte
				binary.LittleEndian.PutUint64(h[0:], uint64(idx+1))
				binary.LittleEndian.PutUint64(h[8:], v.ino)
				binary.LittleEndian.PutUint32(h[16:], uint32(nameLen))
				h[20] = v.ft
				copy(mem[buf+used:buf+used+room], h[:room])
				used += room
			}
			return false
		}
		var h [hdr]byte
		binary.LittleEndian.PutUint64(h[0:], uint64(idx+1)) // d_next
		binary.LittleEndian.PutUint64(h[8:], v.ino)
		binary.LittleEndian.PutUint32(h[16:], uint32(nameLen))
		h[20] = v.ft
		copy(mem[buf+used:], h[:])
		used += hdr
		// Name.
		room := bufLen - used
		if room <= 0 {
			return false
		}
		n := nameLen
		if n > room {
			n = room
		}
		copy(mem[buf+used:buf+used+n], v.name[:n])
		used += n
		return n == nameLen
	}

	virtual := []vent{
		{name: ".", ino: dirIno(hostPath, "."), ft: filetypeDirectory},
		{name: "..", ino: dirIno(hostPath, ".."), ft: filetypeDirectory},
	}
	for _, de := range entries {
		ft := byte(filetypeRegularFile)
		if de.IsDir() {
			ft = filetypeDirectory
		} else if de.Type()&os.ModeSymlink != 0 {
			ft = filetypeSymbolicLink
		}
		var ino uint64
		if info, err := de.Info(); err == nil {
			_, ino, _ = statIdentity(info)
		}
		virtual = append(virtual, vent{name: de.Name(), ino: ino, ft: ft})
	}

	for i := int64(cookie); i < int64(len(virtual)); i++ {
		if !emit(i, virtual[i]) {
			s.putU32(bufUsedPtr, uint32(used))
			return wasiESUCCESS
		}
	}
	s.putU32(bufUsedPtr, uint32(used))
	return wasiESUCCESS
}

// ---- stat identity helpers ------------------------------------------------

// statIdentity extracts (dev, ino, nlink) from an os.FileInfo. On unix it reads
// the underlying *syscall.Stat_t; elsewhere it returns zeros (DuckDB does not
// depend on these for reading data files).
func statIdentity(fi os.FileInfo) (dev, ino, nlink uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink)
	}
	return 0, 0, 0
}

// dirIno best-effort returns the inode of the directory itself (".") or its
// parent (".."). Failures yield 0, which is acceptable for readdir consumers.
func dirIno(hostPath, which string) uint64 {
	target := hostPath
	if which == ".." {
		target = filepath.Dir(hostPath)
	}
	if fi, err := os.Stat(target); err == nil {
		_, ino, _ := statIdentity(fi)
		return ino
	}
	return 0
}
