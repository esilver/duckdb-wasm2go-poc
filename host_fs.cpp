// host_fs.cpp — Tier 2 "Path B": a custom DuckDB FileSystem compiled INTO the
// standalone wasm whose virtual methods call IMPORTED host functions. Under
// -sSTANDALONE_WASM + -sERROR_ON_UNDEFINED_SYMBOLS=0 the undefined extern "C"
// host_* symbols below become clean `env.host_*` wasm imports that the pure-Go
// host (converge/wasishim/hostfs.go) implements against the `os` package.
//
// This gives the CGO_ENABLED=0, wasm2go-transpiled DuckDB REAL host disk access:
// duckdb_open("/tmp/x.duckdb") persists & reopens, read_csv_auto('/path') works.
//
// Why this works (verified against duckdb v1.5.3 source):
//   - DatabaseInstance::GetFileSystem() returns a DatabaseFileSystem, an
//     OpenerFileSystem that forwards every call to *config.file_system, a
//     VirtualFileSystem (database.cpp:398, opener_file_system.hpp).
//   - VirtualFileSystem::FindFileSystemInternal iterates registered subsystems
//     and, if one CanHandleFile(path) AND IsManuallySet(), returns it IMMEDIATELY
//     in preference to the default (broken-in-wasm) LocalFileSystem
//     (virtual_file_system.cpp:400-412).
//   - Both the main DB-file open (SingleFileBlockManager -> FileSystem::Get ->
//     db_file_system -> VFS) and read_csv (same VFS) therefore dispatch to us.
//   - register_host_fs() casts config.file_system to VirtualFileSystem& exactly
//     as database.cpp:398 does, then RegisterSubSystem(make_uniq<HostFileSystem>).

#include "duckdb.hpp"
#include "duckdb/main/capi/capi_internal.hpp"
#include "duckdb/common/file_system.hpp"
#include "duckdb/common/virtual_file_system.hpp"
// FileSystem TRACE logging (CALL enable_logging('FileSystem')): the same
// DUCKDB_LOG_FILE_SYSTEM_* macros LocalFileSystem fires; the handle's logger is
// attached in OpenFile via FileHandle::TryAddLogger(opener).
#include "duckdb/logging/file_system_logger.hpp"
// FileOpener::TryGetCurrentSetting (file_search_path glob resolution).
#include "duckdb/common/file_opener.hpp"
// duckdb::Glob(s, slen, pattern, plen): the engine's own segment matcher
// (*, ?, [..], \escape) — reused so host-FS glob semantics match LocalFileSystem
// exactly.
#include "duckdb/function/scalar/string_common.hpp"

#include <cstdint>
#include <cstring>
#include <queue>
#include <string>
#include <vector>

// ---- IMPORTED host functions (become env.host_* wasm imports) ---------------
// Kept to plain ints/pointers (no by-value structs). All host fds are >= 0 on
// success; negative returns are -errno. Pointers are wasm linear-memory offsets
// the Go side reads/writes via the module's exported memory.
extern "C" {
// Open `path` (len bytes, NOT necessarily NUL-terminated) with our own flag bits
// (see HOSTO_* below). Returns a host fd (>=0) or -errno.
int64_t host_open(const char *path, int32_t pathlen, int32_t flags);
// pread/pwrite at absolute offset. Return bytes transferred (>=0) or -errno.
int64_t host_pread(int32_t fd, void *buf, int64_t n, int64_t off);
int64_t host_pwrite(int32_t fd, const void *buf, int64_t n, int64_t off);
// File size of an open fd, or -errno.
int64_t host_size(int32_t fd);
// Close fd. 0 on success or -errno.
int32_t host_close(int32_t fd);
// Existence check by path. 1 = exists (regular file), 0 = no, -errno on error.
int32_t host_exists(const char *path, int32_t pathlen);
// Truncate fd to n bytes. 0 or -errno.
int32_t host_trunc(int32_t fd, int64_t n);
// Last-modified time of fd, in seconds since the unix epoch (or -errno).
int64_t host_mtime(int32_t fd);
// Flush fd to stable storage (fsync). 0 or -errno.
int32_t host_sync(int32_t fd);
// Remove a file by path. 0 on success or -errno.
int32_t host_unlink(const char *path, int32_t pathlen);
// Rename old->new (atomic). 0 on success or -errno.
int32_t host_rename(const char *oldp, int32_t oldlen, const char *newp, int32_t newlen);
// Directory existence: 1 = dir, 0 = not a dir, -errno on error.
int32_t host_isdir(const char *path, int32_t pathlen);
// Create a directory (with parents). 0 on success or -errno.
int32_t host_mkdir(const char *path, int32_t pathlen);
// Remove a directory tree (recursive; DuckDB's RemoveDirectory semantics).
// 0 on success or -errno.
int32_t host_rmdir(const char *path, int32_t pathlen);
// List directory entries into out as newline-terminated names, with directory
// names suffixed '/'. Returns bytes written (>=0) or -errno (-ERANGE if the
// listing exceeds outcap).
int32_t host_listdir(const char *path, int32_t pathlen, char *out, int32_t outcap);
}

// Open-flag bits we hand the host (decoupled from DuckDB's FileOpenFlags).
enum {
	HOSTO_READ  = 1 << 0,
	HOSTO_WRITE = 1 << 1,
	HOSTO_CREATE = 1 << 2,  // create if missing
	HOSTO_TRUNC = 1 << 3,   // truncate to zero on open
	HOSTO_PRIVATE = 1 << 4, // create with 0600 (FILE_FLAGS_PRIVATE, persistent secrets)
	HOSTO_RDLOCK = 1 << 5,  // FileLockType::READ_LOCK  -> host takes a shared flock
	HOSTO_WRLOCK = 1 << 6,  // FileLockType::WRITE_LOCK -> host takes an exclusive flock
};

// Sentinel "errno" values host_open returns for lock failures (real errnos are
// small; these are well clear of every platform's range). Must match the
// hosteLock* constants in converge/wasishim/hostfs.go.
enum {
	HOSTE_LOCK = 9000,        // conflicting lock held (native: fcntl F_SETLK EAGAIN)
	HOSTE_LOCK_RDOK = 9001,   // conflict, but a shared (read) lock would succeed
	HOSTE_LOCK_NOTSUP = 9002, // file system does not support locks (write-lock case)
};

namespace duckdb {

// ---- HostFileHandle ---------------------------------------------------------
class HostFileHandle : public FileHandle {
public:
	HostFileHandle(FileSystem &fs, const string &path, FileOpenFlags flags, int32_t fd)
	    : FileHandle(fs, path, flags), fd(fd), position(0) {
	}
	~HostFileHandle() override {
		Close();
	}
	void Close() override {
		if (fd >= 0) {
			host_close(fd);
			fd = -1;
			DUCKDB_LOG_FILE_SYSTEM_CLOSE((*this));
		}
	}

	int32_t fd;
	idx_t position; // handle file pointer (for the offset-tracking Read/Write forms)
};

// ---- HostFileSystem ---------------------------------------------------------
class HostFileSystem : public FileSystem {
public:
	// Opens through the host. We resolve the minimal flag translation needed for
	// (a) a duckdb database file (read+write, create) and (b) a CSV (read-only).
	// Like LocalFileSystem, every path-taking entry point first runs
	// FileSystem::ExpandPath(path, opener): '~' -> the home_directory setting
	// (via the opener) or $HOME, and file:/ URLs are stripped to plain paths.
	unique_ptr<FileHandle> OpenFile(const string &path_p, FileOpenFlags flags,
	                                optional_ptr<FileOpener> opener) override {
		auto path = ExpandPath(path_p, opener);
		int32_t hflags = 0;
		if (flags.OpenForReading()) {
			hflags |= HOSTO_READ;
		}
		if (flags.OpenForWriting()) {
			hflags |= HOSTO_WRITE;
		}
		if (flags.CreateFileIfNotExists() || flags.OverwriteExistingFile()) {
			hflags |= HOSTO_CREATE;
		}
		if (flags.OverwriteExistingFile()) {
			hflags |= HOSTO_TRUNC;
		}
		if (flags.CreatePrivateFile()) {
			hflags |= HOSTO_PRIVATE;
		}
		if (hflags == 0) {
			hflags = HOSTO_READ;
		}
		// Native LocalFileSystem::OpenFileExtended takes an OS file lock right
		// after open (fcntl F_SETLK, local_file_system.cpp): WRITE_LOCK for a
		// read-write database file, READ_LOCK for a read-only one. Forward the
		// lock type to the host, which takes a real flock (shared/exclusive) so
		// a second engine instance — same process OR another process — fails the
		// open instead of silently double-writing the file (duckdb-go-pure #5).
		if (flags.Lock() == FileLockType::READ_LOCK) {
			hflags |= HOSTO_RDLOCK;
		} else if (flags.Lock() == FileLockType::WRITE_LOCK) {
			hflags |= HOSTO_WRLOCK;
		}
		int64_t fd = host_open(path.c_str(), (int32_t)path.size(), hflags);
		if (fd < 0) {
			int err = (int)-fd;
			if (err == HOSTE_LOCK || err == HOSTE_LOCK_RDOK || err == HOSTE_LOCK_NOTSUP) {
				// Match native's error shape (local_file_system.cpp:451) — the
				// file EXISTS but is locked, so this must throw even under
				// FILE_FLAGS_NULL_IF_NOT_EXISTS (read-only opens carry it).
				string extended_error;
				if (err == HOSTE_LOCK_NOTSUP) {
					extended_error = "File locks are not supported for this file system, cannot open the "
					                 "file in read-write mode. Try opening the file in read-only mode";
				} else {
					extended_error = "Conflicting lock is held on the file by another DuckDB instance";
					if (err == HOSTE_LOCK_RDOK) {
						extended_error += ". However, you would be able to open this database in "
						                  "read-only mode, e.g. by using the -readonly parameter in the CLI";
					}
				}
				extended_error += ". See also https://duckdb.org/docs/stable/connect/concurrency";
				throw IOException("Could not set lock on file \"%s\": %s", path, extended_error);
			}
			if (flags.ReturnNullIfNotExists()) {
				return nullptr;
			}
			throw IOException("HostFileSystem: failed to open \"%s\" (errno %d)", path, (int)-fd);
		}
		auto handle = make_uniq<HostFileHandle>(*this, path, flags, (int32_t)fd);
		if (opener) {
			// Attach the FileSystem logger (enable_logging('FileSystem')) and
			// log the OPEN, like LocalFileSystem::OpenFile.
			handle->TryAddLogger(*opener);
			DUCKDB_LOG_FILE_SYSTEM_OPEN((*handle));
		}
		if (flags.OpenForAppending()) {
			// Native opens with O_APPEND: every position-form Write lands at EOF.
			// The WAL relies on this (WriteAheadLog::Initialize opens APPEND and
			// BufferedFileWriter writes position-form); starting at 0 would
			// OVERWRITE the WAL head on re-attach (wal_promote_version corpus
			// proof, checkpoint lane 2026-06-10). Emulate by starting the handle
			// position at the current file size (single-writer;
			// BufferedFileWriter::Truncate re-Seeks on truncate).
			int64_t sz = host_size((int32_t)fd);
			if (sz > 0) {
				handle->position = (idx_t)sz;
			}
		}
		return handle;
	}

	// ----- absolute-offset Read/Write (the StorageManager hot path) -----------
	void Read(FileHandle &handle, void *buffer, int64_t nr_bytes, idx_t location) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t got = host_pread(h.fd, buffer, nr_bytes, (int64_t)location);
		if (got < 0) {
			throw IOException("HostFileSystem: read failed on \"%s\" (errno %d)", handle.path, (int)-got);
		}
		if (got != nr_bytes) {
			throw IOException("HostFileSystem: short read on \"%s\" (%lld/%lld)", handle.path,
			                  (long long)got, (long long)nr_bytes);
		}
		DUCKDB_LOG_FILE_SYSTEM_READ(handle, nr_bytes, location);
	}
	void Write(FileHandle &handle, void *buffer, int64_t nr_bytes, idx_t location) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t put = host_pwrite(h.fd, buffer, nr_bytes, (int64_t)location);
		if (put < 0) {
			throw IOException("HostFileSystem: write failed on \"%s\" (errno %d)", handle.path, (int)-put);
		}
		if (put != nr_bytes) {
			throw IOException("HostFileSystem: short write on \"%s\" (%lld/%lld)", handle.path,
			                  (long long)put, (long long)nr_bytes);
		}
		DUCKDB_LOG_FILE_SYSTEM_WRITE(handle, nr_bytes, location);
	}

	// ----- position-tracking Read/Write (CSV scanner hot path) ----------------
	int64_t Read(FileHandle &handle, void *buffer, int64_t nr_bytes) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t got = host_pread(h.fd, buffer, nr_bytes, (int64_t)h.position);
		if (got < 0) {
			throw IOException("HostFileSystem: read failed on \"%s\" (errno %d)", handle.path, (int)-got);
		}
		DUCKDB_LOG_FILE_SYSTEM_READ(handle, got, h.position);
		h.position += (idx_t)got;
		return got;
	}
	int64_t Write(FileHandle &handle, void *buffer, int64_t nr_bytes) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t put = host_pwrite(h.fd, buffer, nr_bytes, (int64_t)h.position);
		if (put < 0) {
			throw IOException("HostFileSystem: write failed on \"%s\" (errno %d)", handle.path, (int)-put);
		}
		DUCKDB_LOG_FILE_SYSTEM_WRITE(handle, put, h.position);
		h.position += (idx_t)put;
		return put;
	}

	int64_t GetFileSize(FileHandle &handle) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t sz = host_size(h.fd);
		if (sz < 0) {
			throw IOException("HostFileSystem: stat failed on \"%s\" (errno %d)", handle.path, (int)-sz);
		}
		return sz;
	}

	timestamp_t GetLastModifiedTime(FileHandle &handle) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t secs = host_mtime(h.fd);
		if (secs < 0) {
			secs = 0;
		}
		return Timestamp::FromEpochSeconds(secs);
	}

	FileType GetFileType(FileHandle &handle) override {
		return FileType::FILE_TYPE_REGULAR;
	}

	void Truncate(FileHandle &handle, int64_t new_size) override {
		auto &h = handle.Cast<HostFileHandle>();
		int32_t rc = host_trunc(h.fd, new_size);
		if (rc < 0) {
			throw IOException("HostFileSystem: truncate failed on \"%s\" (errno %d)", handle.path, (int)-rc);
		}
	}

	// FileSync: flush to disk. host_pwrite already writes THROUGH the syscall to
	// the kernel, so within one process (and across our close/reopen) the data is
	// already visible; we ask the host to fsync for real durability.
	void FileSync(FileHandle &handle) override {
		auto &h = handle.Cast<HostFileHandle>();
		host_sync(h.fd);
	}

	// ----- seek / position bookkeeping ----------------------------------------
	void Seek(FileHandle &handle, idx_t location) override {
		handle.Cast<HostFileHandle>().position = location;
	}
	idx_t SeekPosition(FileHandle &handle) override {
		return handle.Cast<HostFileHandle>().position;
	}
	void Reset(FileHandle &handle) override {
		handle.Cast<HostFileHandle>().position = 0;
	}
	bool CanSeek() override {
		return true;
	}
	bool OnDiskFile(FileHandle &handle) override {
		return true;
	}
	bool IsLocalFileSystem() const override {
		return true;
	}

	// ----- existence ----------------------------------------------------------
	bool FileExists(const string &filename_p, optional_ptr<FileOpener> opener) override {
		auto filename = ExpandPath(filename_p, opener);
		return host_exists(filename.c_str(), (int32_t)filename.size()) == 1;
	}
	bool DirectoryExists(const string &directory_p, optional_ptr<FileOpener> opener) override {
		auto directory = ExpandPath(directory_p, opener);
		return host_isdir(directory.c_str(), (int32_t)directory.size()) == 1;
	}

	// ----- path-based file lifecycle (WAL create/rename/delete on checkpoint) --
	void RemoveFile(const string &filename_p, optional_ptr<FileOpener> opener) override {
		auto filename = ExpandPath(filename_p, opener);
		int32_t rc = host_unlink(filename.c_str(), (int32_t)filename.size());
		if (rc < 0) {
			throw IOException("HostFileSystem: remove failed on \"%s\" (errno %d)", filename, (int)-rc);
		}
	}
	bool TryRemoveFile(const string &filename_p, optional_ptr<FileOpener> opener) override {
		auto filename = ExpandPath(filename_p, opener);
		int32_t rc = host_unlink(filename.c_str(), (int32_t)filename.size());
		return rc == 0;
	}
	void MoveFile(const string &source_p, const string &target_p, optional_ptr<FileOpener> opener) override {
		auto source = ExpandPath(source_p, opener);
		auto target = ExpandPath(target_p, opener);
		int32_t rc = host_rename(source.c_str(), (int32_t)source.size(), target.c_str(),
		                         (int32_t)target.size());
		if (rc < 0) {
			throw IOException("HostFileSystem: rename \"%s\" -> \"%s\" failed (errno %d)", source, target,
			                  (int)-rc);
		}
	}

	// ----- directories (DuckDB's temp-spill dir, EXPORT DATABASE, ...) ---------
	void CreateDirectory(const string &directory_p, optional_ptr<FileOpener> opener) override {
		auto directory = ExpandPath(directory_p, opener);
		int32_t rc = host_mkdir(directory.c_str(), (int32_t)directory.size());
		if (rc < 0) {
			throw IOException("HostFileSystem: mkdir \"%s\" failed (errno %d)", directory, (int)-rc);
		}
	}
	void RemoveDirectory(const string &directory_p, optional_ptr<FileOpener> opener) override {
		auto directory = ExpandPath(directory_p, opener);
		int32_t rc = host_rmdir(directory.c_str(), (int32_t)directory.size());
		if (rc < 0) {
			throw IOException("HostFileSystem: rmdir \"%s\" failed (errno %d)", directory, (int)-rc);
		}
	}
	bool ListFiles(const string &directory_p, const std::function<void(const string &, bool)> &callback,
	               FileOpener *opener) override {
		auto directory = ExpandPath(directory_p, opener);
		std::vector<char> buf(1 << 20);
		int32_t n = host_listdir(directory.c_str(), (int32_t)directory.size(), buf.data(), (int32_t)buf.size());
		if (n < 0) {
			return false;
		}
		size_t start = 0;
		for (int32_t i = 0; i < n; i++) {
			if (buf[i] != '\n') {
				continue;
			}
			string name(buf.data() + start, (size_t)i - start);
			start = (size_t)i + 1;
			if (name.empty()) {
				continue;
			}
			bool is_dir = name.back() == '/';
			if (is_dir) {
				name.pop_back();
			}
			callback(name, is_dir);
		}
		return true;
	}

	// ----- glob ----------------------------------------------------------------
	// Mirrors LocalFileSystem's LocalGlobResult walk (amalg/duckdb.cpp): the path
	// is split on separators; non-glob components are appended literally, glob
	// components ('*', '?', '[..]') are matched against host_listdir entries with
	// the engine's own duckdb::Glob matcher, and '**' crawls recursively (also
	// matching zero directory levels). Relative paths resolve against the process
	// cwd on the Go side, like every other host_* call. Expansion order uses the
	// same smallest-path-first priority queue as LocalGlobResult; within one
	// directory, os.ReadDir (Go side) yields names sorted.
	vector<OpenFileInfo> Glob(const string &path_p, FileOpener *opener) override {
		// like LocalGlobResult: the pattern is ExpandPath'd before splitting, so
		// '~/x*.csv' resolves against the home_directory setting and file:/ URLs
		// glob like plain paths. Results are therefore expanded paths (native
		// behavior: downstream opens use the glob results verbatim).
		auto path = ExpandPath(path_p, opener);
		vector<OpenFileInfo> result;
		if (path.empty()) {
			return result;
		}
		bool absolute = path[0] == '/' || path[0] == '\\';
		if (!FileSystem::HasGlob(path)) {
			// no wildcards: literal path, if it exists; otherwise (relative
			// paths) try each file_search_path entry — LocalFileSystem's
			// FetchFileWithoutGlob (local_file_system.cpp:1575).
			return FetchWithoutGlob(path, opener, absolute);
		}

		// split the path into components (same loop as LocalGlobResult: the first
		// split keeps any leading separator, empty segments are skipped)
		vector<string> splits;
		idx_t last_pos = 0;
		for (idx_t i = 0; i < path.size(); i++) {
			if (path[i] != '\\' && path[i] != '/') {
				continue;
			}
			if (i == last_pos) {
				last_pos = i + 1;
				continue;
			}
			if (splits.empty()) {
				splits.push_back(path.substr(0, i));
			} else {
				splits.push_back(path.substr(last_pos, i - last_pos));
			}
			last_pos = i + 1;
		}
		splits.push_back(path.substr(last_pos));

		idx_t crawl_count = 0;
		for (auto &s : splits) {
			if (s == "**") {
				crawl_count++;
			}
		}
		if (crawl_count > 1) {
			throw IOException("Cannot use multiple '**' in one path");
		}

		// work item: a resolved directory prefix + the index of the next component
		struct ExpandDir {
			string path;
			idx_t split_index;
			bool is_empty; // "no prefix yet" (relative-path start)
			ExpandDir(string p, idx_t si, bool e = false) : path(std::move(p)), split_index(si), is_empty(e) {
			}
			bool operator<(const ExpandDir &other) const {
				return path > other.path; // top() = lexicographically smallest
			}
		};
		std::priority_queue<ExpandDir> work;

		if (absolute) {
			// like LocalGlobResult: no glob support in the FIRST level of an
			// absolute path; start expansion below it
			if (splits.size() > 1) {
				work.emplace(splits[0], (idx_t)1);
			}
		} else {
			// If file_search_path is set, those paths are the first glob
			// elements (local_file_system.cpp:1738) — results keep the search
			// path prefix; cwd is only used when no search path is set.
			Value search_value;
			if (opener && opener->TryGetCurrentSetting("file_search_path", search_value)) {
				auto search_paths = StringUtil::Split(search_value.ToString(), ',');
				for (const auto &search_path : search_paths) {
					work.emplace(search_path, (idx_t)0);
				}
			}
			if (work.empty()) {
				work.emplace(".", (idx_t)0, true);
			}
		}

		while (!work.empty()) {
			ExpandDir dir = work.top();
			work.pop();
			const string &component = splits[dir.split_index];
			bool is_last = dir.split_index + 1 == splits.size();
			if (!FileSystem::HasGlob(component)) {
				// literal component: append as-is
				if (dir.is_empty) {
					work.emplace(component, dir.split_index + 1);
				} else if (is_last) {
					string filename = JoinPath(dir.path, component);
					if (FileExists(filename, opener) || DirectoryExists(filename, opener)) {
						result.emplace_back(std::move(filename));
					}
				} else {
					work.emplace(JoinPath(dir.path, component), dir.split_index + 1);
				}
				continue;
			}
			if (component == "**") {
				if (!is_last) {
					// '**' also matches zero levels (dir/**/f also matches dir/f)
					work.emplace(dir.path, dir.split_index + 1);
				}
				ListFiles(
				    dir.path,
				    [&](const string &name, bool is_dir) {
					    string full = JoinPath(dir.path, name);
					    if (is_dir) {
						    work.emplace(std::move(full), dir.split_index); // keep crawling
					    } else if (is_last) {
						    result.emplace_back(std::move(full));
					    }
				    },
				    opener);
				continue;
			}
			// plain glob component: match this directory level with the engine's
			// matcher. Last component matches files, inner components directories
			// (same as LocalFileSystem's GlobFilesInternal).
			ListFiles(
			    dir.path,
			    [&](const string &name, bool is_dir) {
				    if (is_dir == is_last) {
					    return;
				    }
				    if (!duckdb::Glob(name.c_str(), name.size(), component.c_str(), component.size())) {
					    return;
				    }
				    string full = JoinPath(dir.path, name);
				    if (is_last) {
					    result.emplace_back(std::move(full));
				    } else {
					    work.emplace(std::move(full), dir.split_index + 1);
				    }
			    },
			    opener);
		}

		if (result.empty()) {
			// last-ditch effort (mirrors LocalGlobResult::ExpandNextPath): search
			// the pattern as a string literal (incl. file_search_path joins)
			result = FetchWithoutGlob(path, opener, absolute);
		}
		return result;
	}

	// FetchWithoutGlob ports LocalFileSystem::FetchFileWithoutGlob: the literal
	// path if it exists, else (relative paths only) the first-existing joins
	// against the comma-separated file_search_path setting.
	vector<OpenFileInfo> FetchWithoutGlob(const string &path, optional_ptr<FileOpener> opener, bool absolute_path) {
		vector<OpenFileInfo> result;
		if (host_exists(path.c_str(), (int32_t)path.size()) == 1) {
			result.emplace_back(path);
		} else if (!absolute_path) {
			Value value;
			if (opener && opener->TryGetCurrentSetting("file_search_path", value)) {
				auto search_paths = StringUtil::Split(value.ToString(), ',');
				for (const auto &search_path : search_paths) {
					auto joined_path = JoinPath(search_path, path);
					if (host_exists(joined_path.c_str(), (int32_t)joined_path.size()) == 1) {
						result.emplace_back(joined_path);
					}
				}
			}
		}
		return result;
	}

	// ----- canonicalization (ATTACH path dedup) --------------------------------
	// DatabaseManager::AttachDatabase canonicalizes every attach path and uses the
	// result to detect "database is already attached" (database_manager.cpp:117).
	// Native LocalFileSystem::CanonicalizePath = ExpandPath + make-absolute +
	// realpath. We mirror it minus realpath (no symlink resolution host call):
	// expansion + cwd-absolutization + the engine's textual ./.. removal is
	// consistent across all spellings of the same path, which is what the dedup
	// needs ('~/x.db' vs '<testdir>/x.db' vs 'file://<testdir>/x.db').
	string CanonicalizePath(const string &path_p, optional_ptr<FileOpener> opener) override {
		auto path = ExpandPath(path_p, opener);
		if (path.empty()) {
			return path;
		}
		if (!FileSystem::IsPathAbsolute(path)) {
			path = JoinPath(FileSystem::GetWorkingDirectory(), path);
		}
		return FileSystem::CanonicalizePath(path);
	}

	// ----- dispatch hooks -------------------------------------------------------
	// HostFileSystem is installed as the VFS *default* filesystem (see
	// host_fs_attach_to_config below), exactly where native DuckDB puts its
	// LocalFileSystem; CanHandleFile/IsManuallySet are kept for the legacy
	// register_host_fs() subsystem path only.
	bool CanHandleFile(const string &fpath) override {
		if (fpath.rfind("file:", 0) == 0) {
			return true;
		}
		// Reject protocol-prefixed paths (scheme followed by "://").
		auto pos = fpath.find("://");
		if (pos != string::npos) {
			return false;
		}
		return true;
	}
	bool IsManuallySet() override {
		return true;
	}

	// The engine identifies the local filesystem BY NAME: SET disabled_filesystems
	// ='LocalFileSystem' disables whatever GetName()=="LocalFileSystem"
	// (virtual_file_system.cpp FindFileSystem + LocalDatabaseFileSystem::
	// GetFileSystem), and error messages quote it. We ARE the local filesystem of
	// this build, so we take the native name; the IOException prefixes below keep
	// "HostFileSystem:" for origin debugging.
	std::string GetName() const override {
		return "LocalFileSystem";
	}
};

} // namespace duckdb

// ---- registration -----------------------------------------------------------
// The database FILE is opened DURING duckdb_open (StorageManager), before any
// post-open hook could run, using config.file_system. So the HostFileSystem must
// be installed on the DBConfig BEFORE open. We do that via a config that the Go
// side passes to duckdb_open_ext:
//
//   cfg = duckdb_create_config(); host_fs_attach_to_config(cfg);
//   duckdb_open_ext(path, &db, cfg, &err);
//
// At open, DuckDB MOVES our config.file_system into the instance (database.cpp:
// 476-477) instead of constructing the default local-only VFS, so every open —
// the DB file AND read_csv — dispatches through us.
//
// host_fs_attach_to_config(duckdb_config) — duckdb_config is a DBConfig* (capi).
extern "C" void host_fs_attach_to_config(duckdb_config config) {
	auto *cfg = reinterpret_cast<duckdb::DBConfig *>(config);
	// A VirtualFileSystem whose DEFAULT filesystem is the HostFileSystem — the
	// exact slot native DuckDB gives its LocalFileSystem. Putting it in the
	// default slot (instead of registering a subsystem) matters beyond dispatch:
	// ResolveLocalFileSystem (database.cpp) hands FileSystem::GetLocal(db) the VFS
	// default when IsLocalFileSystem() — that is the path persistent secrets and
	// extension metadata take, which previously fell through to the
	// emscripten-stubbed LocalFileSystem (mkdir -> "Function not implemented").
	auto vfs = duckdb::make_uniq<duckdb::VirtualFileSystem>(duckdb::make_uniq<duckdb::HostFileSystem>());
	cfg->file_system = std::move(vfs);
}

// register_host_fs(duckdb_database) — post-open variant, kept for read_csv-only
// use (registering a subsystem on the already-built instance's VFS). NOT used
// for the DB-file-open path since that is too late; see above.
extern "C" void register_host_fs(duckdb_database db) {
	auto *wrapper = reinterpret_cast<duckdb::DatabaseWrapper *>(db);
	auto &instance = *wrapper->database->instance;
	// config.file_system IS the VirtualFileSystem (database.cpp:398).
	auto &vfs = static_cast<duckdb::VirtualFileSystem &>(*instance.config.file_system);
	vfs.RegisterSubSystem(duckdb::make_uniq<duckdb::HostFileSystem>());
}
