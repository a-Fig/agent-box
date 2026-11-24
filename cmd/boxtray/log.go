package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// maxLogBytes caps boxtray.log; when it grows past this, the older half goes.
const maxLogBytes = 1 << 20

// fileLog is a small append-only log with a size cap. The tray runs with no
// console (a windowsgui build started by Task Scheduler), so this file is the
// only record of why it started, stopped or struggled. It never fails the
// caller: a log that can't be written is dropped, and a nil *fileLog is a
// valid no-op.
type fileLog struct {
	mu   sync.Mutex
	path string
	max  int64
	size int64
	now  func() time.Time
}

// openLog prepares the log at path and trims it if a previous run left it
// over the cap.
func openLog(path string, max int64) *fileLog {
	l := &fileLog{path: path, max: max, now: time.Now}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if fi, err := os.Stat(path); err == nil {
		l.size = fi.Size()
	}
	l.trim()
	return l
}

// Printf appends one line: a timestamp, then the message.
func (l *fileLog) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	line := l.now().Format("2006-01-02T15:04:05.000Z07:00") + " " + fmt.Sprintf(format, args...) + "\n"
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	n, _ := f.WriteString(line)
	f.Close()
	l.size += int64(n)
	l.trim()
}

// trim keeps the newer half of the file, starting at a line boundary, once
// the file is over the cap. Callers hold mu (or are still constructing).
func (l *fileLog) trim() {
	if l.size <= l.max {
		return
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	b = keepNewerHalf(b, l.max)
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) != nil || os.Rename(tmp, l.path) != nil {
		os.Remove(tmp)
		return
	}
	l.size = int64(len(b))
}

// keepNewerHalf returns the last max/2 bytes of b, cut after the first newline
// so no partial line is left at the start.
func keepNewerHalf(b []byte, max int64) []byte {
	keep := int(max / 2)
	if len(b) <= keep {
		return b
	}
	tail := b[len(b)-keep:]
	if i := bytes.IndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return tail
}

// logPath is %LOCALAPPDATA%\agent-box\boxtray.log, or the temp folder if that
// isn't set.
func logPath() string {
	dir := os.Getenv("LOCALAPPDATA")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "agent-box", "boxtray.log")
}
