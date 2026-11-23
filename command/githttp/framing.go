package githttp

import (
	"encoding/binary"
	"io"
	"sync"
	"time"

	"gitlab.com/gitlab-org/gitlab-shell/v14/internal/pktline"
)

const (
	bodyFramingHeader = "Gitlab-Shell-Body-Framing"
	bodyFramingV1     = "v1"

	frameTypeData      byte = 0x01
	frameTypeKeepalive byte = 0x02
	frameHeaderLength       = 5
	maxFrameDataLength      = 64 << 10 // Must match Workhorse's maxSSHBodyFrameDataLength.

	// Cloudflare resets a request whose body is idle for about 15 seconds.
	bodyKeepaliveInterval = 5 * time.Second
)

type bodyFraming bool

func negotiateBodyFraming(version string) bodyFraming {
	return bodyFraming(version == bodyFramingV1)
}

func (b bodyFraming) wrap(fill func(io.Reader, bodyWriter)) func(io.Reader, bodyWriter) {
	if !b {
		return fill
	}
	return func(in io.Reader, w bodyWriter) {
		fill(in, newFramedBodyWriter(w, bodyKeepaliveInterval))
	}
}

func (b bodyFraming) flush() []byte {
	if b {
		return appendFrame(nil, frameTypeData, pktline.PktFlush())
	}
	return pktline.PktFlush()
}

func (b bodyFraming) String() string {
	if b {
		return bodyFramingV1
	}
	return "none"
}

func appendFrame(dst []byte, typ byte, p []byte) []byte {
	dst = append(dst, typ)
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(p))) // #nosec G115 -- p is at most maxFrameDataLength.
	return append(dst, p...)
}

// framedBodyWriter uses the format decoded by Workhorse's sshBodyFrameReader:
// a type byte, a big-endian uint32 length, then the payload. The lock guards
// buffer, lastWrite, writeErr, closed and the timer, and serializes the timer
// goroutine with Write and Close.
// ReplayBody stores framed bytes so retries resend valid frames. Keepalives
// count toward DefaultReplayLimit at about one byte per second.
// Frame headers also count, adding 5 bytes per pkt-line written by
// readUploadPackRequest, about 10% for have/want lines. Negotiations near
// 1 MiB therefore stop being retryable sooner.
type framedBodyWriter struct {
	mu        sync.Mutex
	w         bodyWriter
	interval  time.Duration
	timer     *time.Timer
	lastWrite time.Time
	buffer    []byte
	writeErr  error
	closed    bool
}

func newFramedBodyWriter(w bodyWriter, interval time.Duration) *framedBodyWriter {
	f := &framedBodyWriter{
		w:         w,
		interval:  interval,
		lastWrite: time.Now(),
	}
	f.mu.Lock()
	f.timer = time.AfterFunc(interval, f.keepalive)
	f.mu.Unlock()
	return f
}

func (f *framedBodyWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}

	written := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxFrameDataLength)]
		if err := f.writeFrame(frameTypeData, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

func (f *framedBodyWriter) writeFrame(typ byte, p []byte) error {
	// One Write per frame keeps a frame's header and payload together in the
	// body and costs ReplayBody one lock and wakeup instead of two.
	f.buffer = appendFrame(f.buffer[:0], typ, p)
	_, err := f.w.Write(f.buffer)
	if err != nil {
		f.writeErr = err
		return err
	}
	f.lastWrite = time.Now()
	return nil
}

func (f *framedBodyWriter) keepalive() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.writeErr != nil {
		return
	}
	if elapsed := time.Since(f.lastWrite); elapsed < f.interval {
		f.timer.Reset(f.interval - elapsed)
		return
	}
	if err := f.writeFrame(frameTypeKeepalive, nil); err != nil {
		return
	}
	f.timer.Reset(f.interval)
}

func (f *framedBodyWriter) Close() error {
	return f.CloseWithError(nil)
}

func (f *framedBodyWriter) CloseWithError(err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.timer.Stop()
	return f.w.CloseWithError(err)
}
