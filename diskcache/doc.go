// Package diskcache stores complete encoded jpeg, png, webp, and heic images on disk.
// It does not decode or verify image payloads; callers supply the dimensions and
// format after encoding. Keys must be exactly 64 lowercase hexadecimal digits,
// typically the SHA-256 digest of all transformation inputs and options.
//
// Each entry is a single regular file named <key>.entry. A versioned, fixed
// 64-byte binary header records dimensions, format, creation time, and payload
// size. Get validates the entire header and exact file size before returning a
// payload reader. Malformed, truncated, and expired entries are removed and
// reported as ErrMiss. Get never exposes data read through a symlink.
// Directory-relative operations stay anchored to the opened cache directory.
// Unix uses no-follow, nonblocking opens and file-only unlink operations.
// Other platforms use Lstat and file-identity checks before exposing readers;
// on those platforms callers must prevent adversarial directory mutation during
// removal, since portable file-only unlink is unavailable.
//
// Create reserves a randomly named, exclusive, same-directory temporary file.
// Commit assigns Created and Size, syncs the complete file, and atomically
// renames it into place. Existing readers keep their original file descriptor.
// CommitAndOpen additionally returns a payload descriptor opened before the
// publication, eliminating a publish-then-Get race against eviction or expiry.
// Concurrent processes may generate the same key; the last publication wins,
// and readers see complete old or new entries, never a partly published entry.
// Publication is atomic, not a promise of directory durability after a crash.
// Applications should protect the directory from untrusted writers; the cache
// does not protect image contents from modification by other directory owners.
//
// Zero MaxAge and MaxBytes disable their respective limits. TTL is measured
// from creation, not access. Limits are enforced by Prune (and optionally its
// background timer), not by Commit; Get also rejects expired entries. MaxBytes
// counts header plus payload for valid published entries, not filesystem block
// allocation or temporary files. Prune first removes corrupt/expired entries
// and abandoned temporary files, then evicts oldest-created entries until the
// byte target is met. It ignores unrelated names, directories, and symlinks.
//
// Owned temporary names are .gimg-<64 lowercase hex digits>.tmp. A temp
// file is abandoned only after its modification time is at least 24 hours old.
// Writers active in this Cache are always protected. Other processes' writers
// are not coordinated: a writer idle for 24 hours may be collected. This
// conservative grace period is independent of MaxAge and CleanupInterval.
//
// Close is idempotent. It rejects new operations, joins background cleanup and
// already-started operations, aborts all remaining unpublished writers, and
// releases the directory handle. Close after Commit is safe and does not
// delete the published entry. Open Entry readers remain the caller's property
// and remain usable after cache Close or eviction on Unix. Context cancellation
// is checked at file read/write boundaries; it cannot interrupt a filesystem
// operation already in progress. LastCleanupError reports the most recent
// background cleanup outcome; explicit Prune errors are returned directly.
package diskcache
