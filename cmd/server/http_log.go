package main

import (
	"bytes"
	"io"
	"log"
	"sync"
	"time"
)

// net/http's default logger includes peer addresses, panic values and stacks.
// Retain only bounded fixed classes, without copying request or runtime contents.
type safeHTTPLog struct {
	mu   sync.Mutex
	out  io.Writer
	last map[string]time.Time
	now  func() time.Time
}

func (s *safeHTTPLog) Write(p []byte) (int, error) {
	class := "HTTP server transport error (details suppressed)"
	if bytes.Contains(p, []byte("http: panic serving ")) {
		class = "HTTP handler panic (details suppressed)"
	}
	if bytes.Contains(p, []byte("http: TLS handshake error")) {
		class = "HTTP TLS handshake refused (details suppressed)"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = make(map[string]time.Time)
	}
	now := s.now()
	if previous, ok := s.last[class]; ok && now.Sub(previous) < 30*time.Second {
		return len(p), nil
	}
	s.last[class] = now
	_, err := io.WriteString(s.out, class+"\n")
	return len(p), err
}

func httpErrorLogger(out io.Writer) *log.Logger {
	return log.New(&safeHTTPLog{out: out, now: time.Now}, "", 0)
}
