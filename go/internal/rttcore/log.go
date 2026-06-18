package rttcore

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// logMaxSize is the rotation threshold for the broadcast log (1 MiB), matching
// Python's LOG_MAX_SIZE.
const logMaxSize = 1 * 1024 * 1024

// rotatedMarker is written after a rotation, matching Python's _flush_line.
const rotatedMarker = "[RTT Log rotated]\n"

// logSink is the broadcast, non-draining RTT log: a rotating file every reader
// (rtt_read_log, rtt_read_raw) shares. Unlike the in-memory ring, reading it
// never consumes data, so multiple consumers all see the full output.
type logSink struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

// resolveLogPath picks the log file location, mirroring Python's
// _default_log_file: an explicit RTT_LOG_FILE wins; otherwise a per-platform
// cache/state directory. The directory is created if needed.
func resolveLogPath(override string) (string, error) {
	if override != "" {
		p, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		return p, nil
	}
	base, err := stateDir()
	if err != nil {
		// Last resort: the OS temp directory.
		base = os.TempDir()
	}
	dir := filepath.Join(base, "mcp-rtt-server")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return filepath.Join(os.TempDir(), "mcp-rtt-server-rtt_output.log"), nil
	}
	return filepath.Join(dir, "rtt_output.log"), nil
}

// stateDir returns the per-platform base directory for persistent state/logs,
// approximating Python's XDG_STATE_HOME-on-Linux / %LOCALAPPDATA%-on-Windows.
func stateDir() (string, error) {
	switch runtime.GOOS {
	case "windows", "darwin":
		// Windows: %LOCALAPPDATA%; macOS: ~/Library/Caches.
		return os.UserCacheDir()
	default:
		// Linux: honour XDG_STATE_HOME, falling back to ~/.local/state.
		if v := os.Getenv("XDG_STATE_HOME"); v != "" {
			return v, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state"), nil
	}
}

// openLog truncates and opens the log file for writing (Python opens with 'w').
func openLog(path string) (*logSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &logSink{path: path, f: f}, nil
}

// write appends a stamped line and rotates when the file crosses logMaxSize,
// replicating Python's _flush_line log handling.
func (l *logSink) write(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	n, _ := io.WriteString(l.f, s)
	l.size += int64(n)
	if l.size >= logMaxSize {
		// Close, truncate, reopen, and stamp the rotation marker.
		l.f.Close()
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			l.f = nil
			return
		}
		l.f = f
		l.size = 0
		io.WriteString(l.f, rotatedMarker)
	}
}

func (l *logSink) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// readTail returns up to maxBytes from the end of the log, discarding the
// leading partial line so the result starts on a line boundary (rtt_read_log).
func (l *logSink) readTail(maxBytes int) string {
	size := l.fileSize()
	f, err := os.Open(l.path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if size > int64(maxBytes) {
		f.Seek(-int64(maxBytes), io.SeekEnd)
		// Discard the partial first line.
		readLine(f)
	}
	data, _ := io.ReadAll(f)
	return string(data)
}

// readRaw returns up to maxBytes from offset, with rotation detection: if the
// file shrank below offset (it rotated/truncated), reading restarts at 0. Used
// by rtt_read_raw for non-draining, multi-consumer streaming.
func (l *logSink) readRaw(offset int64, maxBytes int) (string, int64) {
	size := l.fileSize()
	if offset > size {
		offset = 0
	}
	f, err := os.Open(l.path)
	if err != nil {
		return "", 0
	}
	defer f.Close()
	f.Seek(offset, io.SeekStart)
	buf := make([]byte, maxBytes)
	n, _ := io.ReadFull(f, buf)
	if n <= 0 {
		return "", offset
	}
	return string(buf[:n]), offset + int64(n)
}

func (l *logSink) fileSize() int64 {
	info, err := os.Stat(l.path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// readLine reads up to and including the next newline, returning it; the
// partial leading line is then discarded by the caller.
func readLine(f *os.File) {
	var b [1]byte
	for {
		_, err := f.Read(b[:])
		if err != nil {
			return
		}
		if b[0] == '\n' {
			return
		}
	}
}
