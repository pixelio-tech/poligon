package iosscreen

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// A device's WebDriverAgent serves its screen as one long multipart/x-mixed-
// replace response. Reading it is expensive: the bytes cross USB through
// usbmuxd, and WDA re-encodes a screenshot for every frame.
//
// So poligon reads each device exactly once, no matter how many browsers are
// watching. A stream owns the single upstream connection, keeps the latest
// frame, and hands it to every subscriber. Before this, each <img> in the wall
// polled /frame every 90ms and every poll opened a *new* connection to WDA —
// one TCP setup, one mjpeg handshake and one discarded partial stream per
// displayed frame, per tile.

const (
	// how long a stream stays connected after its last viewer leaves, so
	// switching tabs or reloading a page does not restart the upstream
	idleGrace = 15 * time.Second
	// a frame older than this means the device stopped sending
	staleAfter = 5 * time.Second
	maxFrame   = 8 << 20
)

type frame struct {
	data []byte
	at   time.Time
	seq  uint64
}

type stream struct {
	mu     sync.Mutex
	cond   *sync.Cond
	cur    frame
	err    error
	refs   int
	closed bool
	cancel context.CancelFunc
	idle   *time.Timer
	start  time.Time
}

func newStream() *stream {
	s := &stream{start: time.Now()}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// publish stores a frame and wakes every waiting subscriber.
func (s *stream) publish(b []byte) {
	s.mu.Lock()
	s.cur = frame{data: b, at: time.Now(), seq: s.cur.seq + 1}
	s.err = nil
	s.mu.Unlock()
	s.cond.Broadcast()
}

// frames reports how many frames this stream has published so far.
func (s *stream) frames() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.seq
}

func (s *stream) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.cond.Broadcast()
}

// Sub is one viewer of a device's screen.
type Sub struct {
	c    *Controller
	id   string
	s    *stream
	seen uint64
}

// Next blocks until a frame newer than the last one this subscriber saw, and
// returns it. It returns an error if the stream broke or ctx ended.
func (sb *Sub) Next(ctx context.Context) ([]byte, error) {
	s := sb.s

	// wake the waiter when the caller goes away — sync.Cond has no context
	stop := context.AfterFunc(ctx, func() { s.cond.Broadcast() })
	defer stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if s.cur.seq > sb.seen && s.cur.data != nil {
			sb.seen = s.cur.seq
			return s.cur.data, nil
		}
		if s.err != nil {
			return nil, s.err
		}
		if s.closed {
			return nil, errors.New("screen stream closed")
		}
		s.cond.Wait()
	}
}

// Latest returns the most recent frame without waiting, and how old it is.
func (sb *Sub) Latest() ([]byte, time.Duration, error) {
	s := sb.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur.data == nil {
		if s.err != nil {
			return nil, 0, s.err
		}
		return nil, 0, errors.New("no frame yet")
	}
	return s.cur.data, time.Since(s.cur.at), nil
}

// Close drops this viewer. The upstream connection lives on for idleGrace in
// case another viewer arrives.
func (sb *Sub) Close() { sb.c.release(sb.id) }

// Subscribe attaches to a device's screen, starting the shared reader if this
// is the first viewer.
func (c *Controller) Subscribe(deviceID string) (*Sub, error) {
	ep, ok := c.endpoint(deviceID)
	if !ok || ep.MJPEG == "" {
		return nil, fmt.Errorf("no ios screen endpoint for %q", deviceID)
	}

	c.mu.Lock()
	s := c.streams[deviceID]
	if s == nil || s.isClosed() {
		s = newStream()
		c.streams[deviceID] = s
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		go c.pump(ctx, deviceID, ep, s)
	}
	s.mu.Lock()
	s.refs++
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	s.mu.Unlock()
	c.mu.Unlock()

	return &Sub{c: c, id: deviceID, s: s}, nil
}

// StreamDead reports whether someone is watching the device's screen but no
// frame has arrived for longer than after — the reader keeps reconnecting and
// WebDriverAgent's mjpeg server still sends nothing, so WDA itself needs a
// restart even though its /status may answer. False when nobody watches: an
// unwatched screen has no stream to judge.
func (c *Controller) StreamDead(deviceID string, after time.Duration) bool {
	c.mu.Lock()
	s := c.streams[deviceID]
	c.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.refs == 0 {
		return false
	}
	last := s.start
	if s.cur.data != nil {
		last = s.cur.at
	}
	return time.Since(last) > after
}

func (s *stream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// release drops one viewer and arms the idle timer when the last one leaves.
func (c *Controller) release(deviceID string) {
	c.mu.Lock()
	s := c.streams[deviceID]
	c.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.refs--
	if s.refs > 0 || s.idle != nil {
		s.mu.Unlock()
		return
	}
	s.idle = time.AfterFunc(idleGrace, func() { c.stopIfIdle(deviceID, s) })
	s.mu.Unlock()
}

func (c *Controller) stopIfIdle(deviceID string, s *stream) {
	s.mu.Lock()
	if s.refs > 0 {
		s.idle = nil
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	s.cond.Broadcast()
	if cancel != nil {
		cancel()
	}
	c.mu.Lock()
	if c.streams[deviceID] == s {
		delete(c.streams, deviceID)
	}
	c.mu.Unlock()
}

// stopStream tears a device's reader down immediately — used when the endpoint
// changes or goes away, so nobody keeps reading a stale WDA.
func (c *Controller) stopStream(deviceID string) {
	s := c.streams[deviceID] // caller holds c.mu
	if s == nil {
		return
	}
	delete(c.streams, deviceID)
	s.mu.Lock()
	s.closed = true
	cancel := s.cancel
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	s.mu.Unlock()
	s.cond.Broadcast()
	if cancel != nil {
		cancel()
	}
}

// pump keeps one connection to the device's mjpeg server open and republishes
// every frame it reads, reconnecting with backoff while anyone is watching.
func (c *Controller) pump(ctx context.Context, deviceID string, ep Endpoint, s *stream) {
	backoff := 500 * time.Millisecond
	tune := true // only when it can do some good — see below
	for ctx.Err() == nil {
		// Ask WDA to send frames sized for a wall tile. This needs a WDA
		// session, so it is not done on every failed retry: a device that is
		// flapping would otherwise get a new session every few seconds, and a
		// test driving that same WDA could lose the session under it.
		if tune {
			c.tuneMJPEG(deviceID)
			tune = false
		}

		before := s.frames()
		err := c.readMJPEG(ctx, ep, s)
		if ctx.Err() != nil {
			return
		}
		// the connection worked at least once, so a later reconnect is talking
		// to a fresh WDA and has to be told the settings again
		if s.frames() > before {
			tune = true
			backoff = 500 * time.Millisecond
		}
		if err == nil {
			err = io.EOF
		}
		s.fail(fmt.Errorf("ios screen %s: %w", deviceID, err))

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// readMJPEG consumes one multipart response until it breaks.
//
// It does NOT trust the declared boundary. WebDriverAgent announces
// `boundary=--BoundaryString` — the value already contains the two dashes that
// MIME says introduce the delimiter — so a spec-following multipart reader
// hunts for `----BoundaryString`, finds nothing, and reports an empty stream.
// Scanning for JPEG markers is what the old per-frame reader did and it works
// against every mjpeg server we have seen, so that is the only path.
//
// A connection that stays open but stops delivering (the phone locked or
// dozed, the usbmux forward wedged) returns no error and no data, so a plain
// read would wait on it forever while the wall shows a frozen frame. After
// stallAfter without a single byte the request is canceled, which returns
// here and lets pump reconnect.
func (c *Controller) readMJPEG(ctx context.Context, ep Endpoint, s *stream) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallAfter, cancel)
	defer stall.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ep.MJPEG, nil)
	if err != nil {
		return err
	}
	resp, err := c.streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mjpeg: %s", resp.Status)
	}
	err = scanJPEGs(&stallReader{r: resp.Body, t: stall}, s)
	if ctx.Err() != nil && !stall.Stop() {
		return fmt.Errorf("mjpeg: no data for %s, reconnecting", stallAfter)
	}
	return err
}

// stallAfter is how long an open mjpeg connection may go silent.
var stallAfter = 10 * time.Second

// stallReader pushes the stall deadline back on every read that got data.
type stallReader struct {
	r io.Reader
	t *time.Timer
}

func (sr *stallReader) Read(p []byte) (int, error) {
	n, err := sr.r.Read(p)
	if n > 0 {
		sr.t.Reset(stallAfter)
	}
	return n, err
}

var (
	soi = []byte{0xFF, 0xD8}
	eoi = []byte{0xFF, 0xD9}
)

// scanJPEGs splits a boundary-less byte stream on JPEG start/end markers. It
// works on buffered chunks rather than byte by byte, which the old per-frame
// reader did — that cost a function call and a possible slice growth for every
// byte of every frame.
func scanJPEGs(r io.Reader, s *stream) error {
	br := bufio.NewReaderSize(r, 128*1024)
	buf := make([]byte, 0, 512*1024)
	for {
		chunk := make([]byte, 32*1024)
		n, err := br.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
			for {
				i := bytes.Index(buf, soi)
				if i < 0 {
					if len(buf) > maxFrame {
						buf = buf[:0]
					}
					break
				}
				j := bytes.Index(buf[i+2:], eoi)
				if j < 0 {
					if i > 0 {
						buf = append(buf[:0], buf[i:]...)
					}
					if len(buf) > maxFrame {
						buf = buf[:0]
					}
					break
				}
				end := i + 2 + j + 2
				out := make([]byte, end-i)
				copy(out, buf[i:end])
				s.publish(out)
				buf = append(buf[:0], buf[end:]...)
			}
		}
		if err != nil {
			return err
		}
	}
}

// tuneMJPEG re-sends the session settings (mjpeg encoding included) — a WDA
// that restarted under a cached session id comes back with its defaults.
// Errors are not fatal: an older WDA that ignores a setting keeps its default.
func (c *Controller) tuneMJPEG(deviceID string) {
	_ = c.sessionPost(deviceID, "/appium/settings", map[string]any{"settings": c.sessionSettings()})
}
