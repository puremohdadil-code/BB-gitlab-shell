package githttp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gitlab.com/gitlab-org/gitlab-shell/v14/internal/pktline"
)

type testFrame struct {
	typ    byte
	length int
}

func decodeBodyFrames(t *testing.T, body []byte) ([]byte, []testFrame) {
	t.Helper()
	var data []byte
	var frames []testFrame
	for len(body) > 0 {
		require.GreaterOrEqual(t, len(body), frameHeaderLength, "truncated frame header")
		frame := testFrame{typ: body[0], length: int(binary.BigEndian.Uint32(body[1:frameHeaderLength]))}
		body = body[frameHeaderLength:]
		switch frame.typ {
		case frameTypeData:
			require.Positive(t, frame.length)
			require.LessOrEqual(t, frame.length, maxFrameDataLength)
			require.GreaterOrEqual(t, len(body), frame.length, "truncated frame payload")
			data = append(data, body[:frame.length]...)
			body = body[frame.length:]
		case frameTypeKeepalive:
			require.Zero(t, frame.length)
		default:
			t.Fatalf("unknown frame type: 0x%02x", frame.typ)
		}
		frames = append(frames, frame)
	}
	return data, frames
}

type recordingBodyWriter struct {
	t              *testing.T
	mu             sync.Mutex
	buffer         bytes.Buffer
	writes         int
	closed         bool
	closeErr       error
	writeErr       error
	failAtWrite    int
	failAfterBytes int
}

func (w *recordingBodyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !assert.False(w.t, w.closed, "write after close") {
		return 0, io.ErrClosedPipe
	}
	w.writes++
	if w.writeErr != nil && w.writes == w.failAtWrite {
		n, _ := w.buffer.Write(p[:min(w.failAfterBytes, len(p))])
		return n, w.writeErr
	}
	return w.buffer.Write(p)
}

func (w *recordingBodyWriter) Close() error {
	return w.CloseWithError(nil)
}

func (w *recordingBodyWriter) CloseWithError(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.closeErr = err
	return nil
}

func (w *recordingBodyWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}

func (w *recordingBodyWriter) Writes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

type delayedBodyWriter struct {
	bodyWriter
	delay time.Duration
}

func (w *delayedBodyWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.delay = 0
	return w.bodyWriter.Write(p)
}

func TestNegotiateBodyFraming(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		want    bodyFraming
	}{
		{name: "v1", version: "v1", want: true},
		{name: "unsupported", version: "v2"},
		{name: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			framing := negotiateBodyFraming(tc.version)
			assert.Equal(t, tc.want, framing)
		})
	}
}

func TestBodyFraming(t *testing.T) {
	for _, tc := range []struct {
		name      string
		framing   bodyFraming
		wantLog   string
		wantFlush []byte
		wantBody  []byte
	}{
		{name: "off", wantLog: "none", wantFlush: pktline.PktFlush(), wantBody: []byte("git")},
		{
			name:      "on",
			framing:   true,
			wantLog:   "v1",
			wantFlush: []byte{1, 0, 0, 0, 4, '0', '0', '0', '0'},
			wantBody:  []byte{1, 0, 0, 0, 3, 'g', 'i', 't'},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantLog, tc.framing.String())
			assert.Equal(t, tc.wantFlush, tc.framing.flush())
			wrapped := &recordingBodyWriter{t: t}
			tc.framing.wrap(copyRequestBody)(strings.NewReader("git"), wrapped)
			assert.Equal(t, tc.wantBody, wrapped.Bytes())
			assert.True(t, wrapped.closed)
		})
	}
}

func TestFramedBodyWriter_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lengths []int
		frames  []testFrame
	}{
		{name: "one byte", lengths: []int{1}, frames: []testFrame{{frameTypeData, 1}}},
		{name: "below limit", lengths: []int{maxFrameDataLength - 1}, frames: []testFrame{{frameTypeData, maxFrameDataLength - 1}}},
		{name: "at limit", lengths: []int{maxFrameDataLength}, frames: []testFrame{{frameTypeData, maxFrameDataLength}}},
		{name: "above limit", lengths: []int{maxFrameDataLength + 1}, frames: []testFrame{{frameTypeData, maxFrameDataLength}, {frameTypeData, 1}}},
		{
			name:    "several chunks",
			lengths: []int{3*maxFrameDataLength + 1},
			frames: []testFrame{
				{frameTypeData, maxFrameDataLength},
				{frameTypeData, maxFrameDataLength},
				{frameTypeData, maxFrameDataLength},
				{frameTypeData, 1},
			},
		},
		{
			name:    "multiple writes",
			lengths: []int{1, maxFrameDataLength + 1, 2},
			frames:  []testFrame{{frameTypeData, 1}, {frameTypeData, maxFrameDataLength}, {frameTypeData, 1}, {frameTypeData, 2}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := &recordingBodyWriter{t: t}
			writer := newFramedBodyWriter(wrapped, time.Hour)
			defer writer.Close()
			var input []byte
			for index, length := range tc.lengths {
				p := bytes.Repeat([]byte{byte(index)}, length)
				n, err := writer.Write(p)
				require.NoError(t, err)
				require.Equal(t, len(p), n)
				input = append(input, p...)
			}
			data, frames := decodeBodyFrames(t, wrapped.Bytes())
			require.Equal(t, input, data)
			require.Equal(t, tc.frames, frames)
			require.Equal(t, len(tc.frames), wrapped.Writes())
		})
	}
}

func TestFramedBodyWriter_ZeroLengthWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wrapped := &recordingBodyWriter{t: t}
		writer := newFramedBodyWriter(wrapped, bodyKeepaliveInterval)
		defer writer.Close()
		time.Sleep(3 * time.Second)
		n, err := writer.Write(nil)
		require.NoError(t, err)
		require.Zero(t, n)
		require.Empty(t, wrapped.Bytes())
		time.Sleep(2 * time.Second)
		synctest.Wait()
		data, frames := decodeBodyFrames(t, wrapped.Bytes())
		require.Empty(t, data)
		require.Equal(t, []testFrame{{frameTypeKeepalive, 0}}, frames)
	})
}

func TestFramedBodyWriter_Keepalive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wrapped := &recordingBodyWriter{t: t}
		writer := newFramedBodyWriter(wrapped, bodyKeepaliveInterval)
		defer writer.Close()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		require.Equal(t, []byte{2, 0, 0, 0, 0}, wrapped.Bytes())
		time.Sleep(2 * time.Second)
		n, err := writer.Write([]byte("git"))
		require.NoError(t, err)
		require.Equal(t, 3, n)
		afterWrite := wrapped.Bytes()
		for _, gap := range []time.Duration{3 * time.Second, time.Second} {
			time.Sleep(gap)
			synctest.Wait()
			require.Equal(t, afterWrite, wrapped.Bytes())
		}
		for count := 1; count <= 3; count++ {
			gap := 5 * time.Second
			if count == 1 {
				gap = time.Second
			}
			time.Sleep(gap)
			synctest.Wait()
			data, frames := decodeBodyFrames(t, wrapped.Bytes())
			require.Equal(t, "git", string(data))
			wantFrames := []testFrame{{frameTypeKeepalive, 0}, {frameTypeData, 3}}
			for range count {
				wantFrames = append(wantFrames, testFrame{frameTypeKeepalive, 0})
			}
			require.Equal(t, wantFrames, frames)
		}
	})
}

func TestFramedBodyWriter_DataWriteAtKeepaliveDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wrapped := &recordingBodyWriter{t: t}
		writer := newFramedBodyWriter(&delayedBodyWriter{bodyWriter: wrapped, delay: time.Second}, bodyKeepaliveInterval)
		defer writer.Close()
		time.Sleep(bodyKeepaliveInterval - time.Second)
		n, err := writer.Write([]byte("git"))
		require.NoError(t, err)
		require.Equal(t, 3, n)
		synctest.Wait()
		data, frames := decodeBodyFrames(t, wrapped.Bytes())
		require.Equal(t, "git", string(data))
		require.Equal(t, []testFrame{{frameTypeData, 3}}, frames)

		time.Sleep(bodyKeepaliveInterval - time.Nanosecond)
		synctest.Wait()
		_, frames = decodeBodyFrames(t, wrapped.Bytes())
		require.Equal(t, []testFrame{{frameTypeData, 3}}, frames)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		_, frames = decodeBodyFrames(t, wrapped.Bytes())
		require.Equal(t, []testFrame{{frameTypeData, 3}, {frameTypeKeepalive, 0}}, frames)
	})
}

func TestFramedBodyWriter_NoKeepaliveAfterClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "Close"},
		{name: "CloseWithError", err: errors.New("stdin failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				wrapped := &recordingBodyWriter{t: t}
				writer := newFramedBodyWriter(wrapped, bodyKeepaliveInterval)
				closeWriter := writer.Close
				if tc.err != nil {
					closeWriter = func() error { return writer.CloseWithError(tc.err) }
				}
				time.Sleep(bodyKeepaliveInterval)
				synctest.Wait()
				require.NoError(t, closeWriter())
				afterClose := wrapped.Bytes()
				writes := wrapped.Writes()
				time.Sleep(10 * bodyKeepaliveInterval)
				synctest.Wait()
				require.Equal(t, afterClose, wrapped.Bytes())
				require.True(t, wrapped.closed)
				require.ErrorIs(t, wrapped.closeErr, tc.err)
				n, err := writer.Write([]byte("git"))
				require.ErrorIs(t, err, io.ErrClosedPipe)
				require.Zero(t, n)
				require.Equal(t, writes, wrapped.Writes())
			})
		})
	}
}

func TestFramedBodyWriter_KeepaliveWriteFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writeErr := errors.New("body failed")
		wrapped := &recordingBodyWriter{t: t, writeErr: writeErr, failAtWrite: 1}
		writer := newFramedBodyWriter(wrapped, bodyKeepaliveInterval)
		defer writer.Close()
		time.Sleep(10 * bodyKeepaliveInterval)
		synctest.Wait()
		require.Equal(t, 1, wrapped.Writes())
		require.Empty(t, wrapped.Bytes())
		n, err := writer.Write([]byte("git"))
		require.ErrorIs(t, err, writeErr)
		require.Zero(t, n)
		time.Sleep(10 * bodyKeepaliveInterval)
		synctest.Wait()
		require.Equal(t, 1, wrapped.Writes())
		require.Empty(t, wrapped.Bytes())
	})
}

func TestFramedBodyWriter_WriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name           string
		failAtWrite    int
		failAfterBytes int
		wantWritten    int
	}{
		{name: "first frame", failAtWrite: 1},
		{name: "second frame partial payload", failAtWrite: 2, failAfterBytes: 6, wantWritten: maxFrameDataLength},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				writeErr := errors.New("body failed")
				wrapped := &recordingBodyWriter{t: t, writeErr: writeErr, failAtWrite: tc.failAtWrite, failAfterBytes: tc.failAfterBytes}
				writer := newFramedBodyWriter(wrapped, bodyKeepaliveInterval)
				defer writer.Close()
				n, err := writer.Write(bytes.Repeat([]byte("x"), 2*maxFrameDataLength+1))
				require.ErrorIs(t, err, writeErr)
				require.Equal(t, tc.wantWritten, n)
				afterFailure := wrapped.Bytes()
				n, err = writer.Write([]byte("retry"))
				require.ErrorIs(t, err, writeErr)
				require.Zero(t, n)
				time.Sleep(10 * bodyKeepaliveInterval)
				synctest.Wait()
				require.Equal(t, tc.failAtWrite, wrapped.Writes())
				require.Equal(t, afterFailure, wrapped.Bytes())
			})
		})
	}
}

func TestFramedBodyWriter_ConcurrentWritesAndKeepalives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wrapped := &recordingBodyWriter{t: t}
		writer := newFramedBodyWriter(wrapped, bodyKeepaliveInterval)
		defer writer.Close()
		type writeResult struct {
			n   int
			err error
		}
		results := make(chan writeResult, 30)
		var workers sync.WaitGroup
		for _, gap := range []time.Duration{4 * time.Second, 5 * time.Second, 6 * time.Second} {
			workers.Go(func() {
				for range 10 {
					time.Sleep(gap)
					n, err := writer.Write(bytes.Repeat([]byte("x"), maxFrameDataLength+1))
					results <- writeResult{n: n, err: err}
				}
			})
		}
		workers.Wait()
		require.NoError(t, writer.Close())
		close(results)
		for result := range results {
			require.NoError(t, result.err)
			require.Equal(t, maxFrameDataLength+1, result.n)
		}
		data, frames := decodeBodyFrames(t, wrapped.Bytes())
		require.Equal(t, bytes.Repeat([]byte("x"), 3*10*(maxFrameDataLength+1)), data)
		keepalives := 0
		for _, frame := range frames {
			if frame.typ == frameTypeKeepalive {
				keepalives++
			}
		}
		// synctest's clock makes this deterministic. Every gap between data writes
		// is under bodyKeepaliveInterval except the gap from 54s to 60s. The only
		// keepalive fires at 59s, before the final write and Close at 60s.
		require.Equal(t, 1, keepalives)
	})
}

func TestReadUploadPackRequestWithBodyFraming(t *testing.T) {
	want := pktLine("want e56497bb5f03a90a51293fc6d516788730953899\n")
	done := string(pktline.PktDone())
	t.Run("done with trailing bytes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			wrapped := &recordingBodyWriter{t: t}
			bodyFraming(true).wrap(readUploadPackRequest)(strings.NewReader(want+done+"trailing"), wrapped)
			data, _ := decodeBodyFrames(t, wrapped.Bytes())
			require.Equal(t, want+done+"0000", string(data))
			require.True(t, wrapped.closed)
		})
	})
	t.Run("keepalive while waiting for done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			input, stdin := io.Pipe()
			defer stdin.Close()
			wrapped := &recordingBodyWriter{t: t}
			go bodyFraming(true).wrap(readUploadPackRequest)(input, wrapped)
			_, err := io.WriteString(stdin, want)
			require.NoError(t, err)
			time.Sleep(6 * time.Second)
			_, err = io.WriteString(stdin, done)
			require.NoError(t, err)
			synctest.Wait()
			data, frames := decodeBodyFrames(t, wrapped.Bytes())
			require.Equal(t, want+done+"0000", string(data))
			require.Equal(t, []testFrame{{frameTypeData, len(want)}, {frameTypeKeepalive, 0}, {frameTypeData, len(done)}, {frameTypeData, 4}}, frames)
			require.True(t, wrapped.closed)
		})
	})
}

func TestFramedReplayBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		input, stdin := io.Pipe()
		defer stdin.Close()
		body, reader := startReplayBody(input, bodyFraming(true).wrap(copyRequestBody))
		defer body.Abort()
		defer reader.Close()
		first := "git commands"
		rest := strings.Repeat("pack", 20000)
		_, err := io.WriteString(stdin, first)
		require.NoError(t, err)
		time.Sleep(bodyKeepaliveInterval)
		synctest.Wait()
		prefix := make([]byte, 2*frameHeaderLength+len(first))
		_, err = io.ReadFull(reader, prefix)
		require.NoError(t, err)
		data, frames := decodeBodyFrames(t, prefix)
		require.Equal(t, first, string(data))
		require.Equal(t, []testFrame{{frameTypeData, len(first)}, {frameTypeKeepalive, 0}}, frames)

		rewinder, ok := reader.(interface{ GetBody() (io.ReadCloser, error) })
		require.True(t, ok)
		replay, err := rewinder.GetBody()
		require.NoError(t, err)
		defer replay.Close()
		_, err = io.WriteString(stdin, rest)
		require.NoError(t, err)
		require.NoError(t, stdin.Close())
		replayed, err := io.ReadAll(replay)
		require.NoError(t, err)
		data, replayedFrames := decodeBodyFrames(t, replayed)
		require.Equal(t, first+rest, string(data))
		require.Equal(t, frames, replayedFrames[:len(frames)])
		remaining, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.Equal(t, append(prefix, remaining...), replayed)
	})
}
