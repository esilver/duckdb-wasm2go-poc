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

#include <cstdint>
#include <cstring>
#include <string>

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
}

// Open-flag bits we hand the host (decoupled from DuckDB's FileOpenFlags).
enum {
	HOSTO_READ  = 1 << 0,
	HOSTO_WRITE = 1 << 1,
	HOSTO_CREATE = 1 << 2, // create if missing
	HOSTO_TRUNC = 1 << 3,  // truncate to zero on open
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
	unique_ptr<FileHandle> OpenFile(const string &path, FileOpenFlags flags,
	                                optional_ptr<FileOpener> opener) override {
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
		if (hflags == 0) {
			hflags = HOSTO_READ;
		}
		int64_t fd = host_open(path.c_str(), (int32_t)path.size(), hflags);
		if (fd < 0) {
			if (flags.ReturnNullIfNotExists()) {
				return nullptr;
			}
			throw IOException("HostFileSystem: failed to open \"{}\" (errno {})", path, (int)-fd);
		}
		return make_uniq<HostFileHandle>(*this, path, flags, (int32_t)fd);
	}

	// ----- absolute-offset Read/Write (the StorageManager hot path) -----------
	void Read(FileHandle &handle, void *buffer, int64_t nr_bytes, idx_t location) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t got = host_pread(h.fd, buffer, nr_bytes, (int64_t)location);
		if (got < 0) {
			throw IOException("HostFileSystem: read failed on \"{}\" (errno {})", handle.path, (int)-got);
		}
		if (got != nr_bytes) {
			throw IOException("HostFileSystem: short read on \"{}\" ({}/{})", handle.path,
			                  (long long)got, (long long)nr_bytes);
		}
	}
	void Write(FileHandle &handle, void *buffer, int64_t nr_bytes, idx_t location) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t put = host_pwrite(h.fd, buffer, nr_bytes, (int64_t)location);
		if (put < 0) {
			throw IOException("HostFileSystem: write failed on \"{}\" (errno {})", handle.path, (int)-put);
		}
		if (put != nr_bytes) {
			throw IOException("HostFileSystem: short write on \"{}\" ({}/{})", handle.path,
			                  (long long)put, (long long)nr_bytes);
		}
	}

	// ----- position-tracking Read/Write (CSV scanner hot path) ----------------
	int64_t Read(FileHandle &handle, void *buffer, int64_t nr_bytes) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t got = host_pread(h.fd, buffer, nr_bytes, (int64_t)h.position);
		if (got < 0) {
			throw IOException("HostFileSystem: read failed on \"{}\" (errno {})", handle.path, (int)-got);
		}
		h.position += (idx_t)got;
		return got;
	}
	int64_t Write(FileHandle &handle, void *buffer, int64_t nr_bytes) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t put = host_pwrite(h.fd, buffer, nr_bytes, (int64_t)h.position);
		if (put < 0) {
			throw IOException("HostFileSystem: write failed on \"{}\" (errno {})", handle.path, (int)-put);
		}
		h.position += (idx_t)put;
		return put;
	}

	int64_t GetFileSize(FileHandle &handle) override {
		auto &h = handle.Cast<HostFileHandle>();
		int64_t sz = host_size(h.fd);
		if (sz < 0) {
			throw IOException("HostFileSystem: stat failed on \"{}\" (errno {})", handle.path, (int)-sz);
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
			throw IOException("HostFileSystem: truncate failed on \"{}\" (errno {})", handle.path, (int)-rc);
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
	bool FileExists(const string &filename, optional_ptr<FileOpener> opener) override {
		return host_exists(filename.c_str(), (int32_t)filename.size()) == 1;
	}
	bool DirectoryExists(const string &directory, optional_ptr<FileOpener> opener) override {
		return host_isdir(directory.c_str(), (int32_t)directory.size()) == 1;
	}

	// ----- path-based file lifecycle (WAL create/rename/delete on checkpoint) --
	void RemoveFile(const string &filename, optional_ptr<FileOpener> opener) override {
		int32_t rc = host_unlink(filename.c_str(), (int32_t)filename.size());
		if (rc < 0) {
			throw IOException("HostFileSystem: remove failed on \"{}\" (errno {})", filename, (int)-rc);
		}
	}
	bool TryRemoveFile(const string &filename, optional_ptr<FileOpener> opener) override {
		int32_t rc = host_unlink(filename.c_str(), (int32_t)filename.size());
		return rc == 0;
	}
	void MoveFile(const string &source, const string &target, optional_ptr<FileOpener> opener) override {
		int32_t rc = host_rename(source.c_str(), (int32_t)source.size(), target.c_str(),
		                         (int32_t)target.size());
		if (rc < 0) {
			throw IOException("HostFileSystem: rename \"{}\" -> \"{}\" failed (errno {})", source, target,
			                  (int)-rc);
		}
	}

	// ----- glob: no wildcard support; pass the literal path through if it exists.
	// (read_csv_auto('/abs/file.csv') globs the literal path first.) ------------
	vector<OpenFileInfo> Glob(const string &path, FileOpener *opener) override {
		vector<OpenFileInfo> result;
		if (host_exists(path.c_str(), (int32_t)path.size()) == 1) {
			result.emplace_back(path);
		}
		return result;
	}

	// ----- dispatch hooks: take precedence over the default LocalFileSystem ----
	// CanHandleFile returns true for plain absolute/relative paths and file: URIs
	// (anything that is NOT another registered protocol like http:// or s3://).
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
	// IsManuallySet() == true makes FindFileSystemInternal return us IMMEDIATELY
	// instead of merely as a candidate, so we win over the default fs.
	bool IsManuallySet() override {
		return true;
	}

	std::string GetName() const override {
		return "HostFileSystem";
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
	// A VirtualFileSystem wrapping a default local fs (so non-file protocols and
	// the registry plumbing still work), with our HostFileSystem registered as a
	// subsystem that wins for plain paths (CanHandleFile + IsManuallySet).
	auto vfs = duckdb::make_uniq<duckdb::VirtualFileSystem>(duckdb::FileSystem::CreateLocal());
	vfs->RegisterSubSystem(duckdb::make_uniq<duckdb::HostFileSystem>());
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
