package agenthistory

import (
	"io/fs"
	"os"
)

// statSourceFingerprint, ported from src/agent-history/source-fingerprint.ts.

// statOrNil returns the FileInfo for path or nil when stat fails, mirroring
// `stat(path).catch(() => null)`.
func statOrNil(path string) fs.FileInfo {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

// StatSourceFingerprint mirrors statSourceFingerprint(path).
//
// The fingerprint folds a SQLite sidecar pair into the main file's values:
// mtimeMs = trunc(main) + trunc(wal)*3 + trunc(shm)*7, size = sum of all sizes.
// Missing sidecars contribute 0, matching the legacy single-file values.
//
// Missing main file (including a broken symlink) yields nil.
func StatSourceFingerprint(path string) *SourceFingerprint {
	main := statOrNil(path)
	if main == nil {
		return nil
	}
	wal := statOrNil(path + "-wal")
	shm := statOrNil(path + "-shm")

	walMtime := float64(0)
	walSize := int64(0)
	if wal != nil {
		walMtime = float64(wal.ModTime().UnixMilli())
		walSize = wal.Size()
	}
	shmMtime := float64(0)
	shmSize := int64(0)
	if shm != nil {
		shmMtime = float64(shm.ModTime().UnixMilli())
		shmSize = shm.Size()
	}

	mtimeMs := float64(main.ModTime().UnixMilli()) + walMtime*3 + shmMtime*7
	return &SourceFingerprint{
		MtimeMs: mtimeMs,
		Path:    path,
		Size:    main.Size() + walSize + shmSize,
	}
}
