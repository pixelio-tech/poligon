package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/iosscreen"
)

// iosControl is the iOS screen's input channel: one WebSocket per open page
// instead of one POST (cookie + CSRF check + new request) per tap. Actions run
// in the order they arrive; each gets a reply with how long WDA took, and once
// a second the server pushes the stream's fps so the page can show both.
//
// It reuses shellUpgrader's same-origin check: GET skips poligon's CSRF check
// and a browser attaches cookies to a cross-origin handshake.
func (s *Server) iosControl(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	conn, err := shellUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(64 << 10)

	type reply struct {
		Type  string `json:"type"`
		Seq   int64  `json:"seq,omitempty"`
		OK    bool   `json:"ok,omitempty"`
		Error string `json:"error,omitempty"`
		MS    int64  `json:"ms,omitempty"`
		FPS   *int   `json:"fps,omitempty"`
		Age   *int64 `json:"age_ms,omitempty"`
	}
	out := make(chan reply, 64)
	done := make(chan struct{})
	defer close(done)

	// single writer: replies and stats share the connection. It closes the
	// connection on the way out, so the reader below stops too.
	wdone := make(chan struct{})
	go func() {
		defer close(wdone)
		defer conn.Close()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		last, _ := s.ios.FrameCount(id)
		for {
			select {
			case <-done:
				return
			case m := <-out:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(m); err != nil {
					return
				}
			case <-t.C:
				// the device may have been released or handed to someone else
				if res, ok, _ := s.res.Holder(id); !ok || res.User != u.Name {
					_ = conn.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(4003, "device released"), time.Now().Add(time.Second))
					return
				}
				m := reply{Type: "stats"}
				if n, ok := s.ios.FrameCount(id); ok {
					fps := int(n - last)
					if n < last {
						fps = 0
					}
					last = n
					m.FPS = &fps
				}
				if age, err := s.ios.FrameAge(id); err == nil {
					ms := age.Milliseconds()
					m.Age = &ms
				}
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if err := conn.WriteJSON(m); err != nil {
					return
				}
			}
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var in struct {
			Seq int64 `json:"seq"`
			iosscreen.Input
		}
		if err := json.Unmarshal(msg, &in); err != nil {
			select {
			case out <- reply{Type: "ack", Seq: in.Seq, Error: err.Error()}:
			case <-wdone:
				return
			}
			continue
		}
		start := time.Now()
		err = s.ios.Do(id, in.Input)
		m := reply{Type: "ack", Seq: in.Seq, OK: err == nil, MS: time.Since(start).Milliseconds()}
		if err != nil {
			m.Error = err.Error()
		}
		select {
		case out <- m:
		case <-wdone:
			return
		}
	}
}
