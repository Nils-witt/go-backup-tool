package notify

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSMTPMessage is one message a fakeSMTPServer accepted, captured for a
// test to assert against.
type fakeSMTPMessage struct {
	authUser string
	from     string
	to       []string
	data     string
}

// fakeSMTPServer is a minimal SMTP server (no TLS, no real delivery) driving
// enough of the protocol for net/smtp's client — and so SendMail — to
// complete a full send: EHLO, AUTH PLAIN, MAIL FROM, RCPT TO, DATA, QUIT.
type fakeSMTPServer struct {
	ln net.Listener

	mu       sync.Mutex
	messages []fakeSMTPMessage
}

func startFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}

	s := &fakeSMTPServer{ln: ln}

	go s.serve()

	t.Cleanup(func() { _ = ln.Close() })

	return s
}

func (s *fakeSMTPServer) hostPort(t *testing.T) (string, int) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort(%q) error: %v", s.ln.Addr().String(), err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi(%q) error: %v", portStr, err)
	}

	return host, port
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}

		go s.handle(conn)
	}
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	tp := textproto.NewConn(conn)

	if err := tp.PrintfLine("220 fake.example.com ESMTP"); err != nil {
		return
	}

	var msg fakeSMTPMessage

	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}

		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			_ = tp.PrintfLine("250-fake.example.com")
			_ = tp.PrintfLine("250 AUTH PLAIN")
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			msg.authUser = fakeSMTPParseAuthPlain(line)
			_ = tp.PrintfLine("235 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			msg.from = fakeSMTPExtractAddr(line)
			_ = tp.PrintfLine("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			msg.to = append(msg.to, fakeSMTPExtractAddr(line))
			_ = tp.PrintfLine("250 OK")
		case upper == "DATA":
			if !s.acceptData(tp, &msg) {
				return
			}
		case upper == "QUIT":
			_ = tp.PrintfLine("221 Bye")
			return
		default:
			_ = tp.PrintfLine("500 unrecognized command")
		}
	}
}

// acceptData drives the DATA command's exchange: the 354 continuation, the
// dot-terminated message body, and the closing 250, then records *msg (with
// its body filled in) to s.messages and resets it for the connection's next
// message. Returns false if the connection failed partway through, telling
// handle to stop reading from it.
func (s *fakeSMTPServer) acceptData(tp *textproto.Conn, msg *fakeSMTPMessage) bool {
	if err := tp.PrintfLine("354 End data with <CR><LF>.<CR><LF>"); err != nil {
		return false
	}

	data, err := io.ReadAll(tp.DotReader())
	if err != nil {
		return false
	}

	msg.data = string(data)

	if err := tp.PrintfLine("250 OK"); err != nil {
		return false
	}

	s.mu.Lock()
	s.messages = append(s.messages, *msg)
	s.mu.Unlock()

	*msg = fakeSMTPMessage{}

	return true
}

// fakeSMTPParseAuthPlain extracts the authentication identity (the second of
// PLAIN's three NUL-separated fields) from an "AUTH PLAIN <base64>" command
// line, or "" if it's malformed in any way — good enough for a test double
// that only needs to see the identity, not enforce the mechanism.
func fakeSMTPParseAuthPlain(line string) string {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return ""
	}

	decoded, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return ""
	}

	segs := strings.Split(string(decoded), "\x00")
	if len(segs) != 3 {
		return ""
	}

	return segs[1]
}

func fakeSMTPExtractAddr(line string) string {
	i := strings.Index(line, "<")
	j := strings.Index(line, ">")

	if i == -1 || j == -1 || j < i {
		return ""
	}

	return line[i+1 : j]
}

func TestSendMailPlainNoAuth(t *testing.T) {
	t.Parallel()

	srv := startFakeSMTPServer(t)
	host, port := srv.hostPort(t)

	cfg := SMTPSettings{Host: host, Port: port, Security: SMTPSecurityNone}

	err := SendMail(context.Background(), cfg, "from@example.com", []string{"to@example.com"}, "Test subject", "line one\nline two")
	if err != nil {
		t.Fatalf("SendMail() error: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if len(srv.messages) != 1 {
		t.Fatalf("server received %d messages, want 1", len(srv.messages))
	}

	m := srv.messages[0]

	if m.from != "from@example.com" {
		t.Errorf("MAIL FROM = %q, want from@example.com", m.from)
	}

	if len(m.to) != 1 || m.to[0] != "to@example.com" {
		t.Errorf("RCPT TO = %v, want [to@example.com]", m.to)
	}

	if !strings.Contains(m.data, "Subject: Test subject") {
		t.Errorf("message data = %q, want it to contain the Subject header", m.data)
	}

	if !strings.Contains(m.data, "line one") || !strings.Contains(m.data, "line two") {
		t.Errorf("message data = %q, want it to contain the body", m.data)
	}
}

func TestSendMailWithAuth(t *testing.T) {
	t.Parallel()

	srv := startFakeSMTPServer(t)
	host, port := srv.hostPort(t)

	cfg := SMTPSettings{Host: host, Port: port, Security: SMTPSecurityNone, Username: "user@example.com", Password: "hunter2"}

	if err := SendMail(context.Background(), cfg, "from@example.com", []string{"a@example.com", "b@example.com"}, "Subj", "body"); err != nil {
		t.Fatalf("SendMail() error: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()

	if len(srv.messages) != 1 {
		t.Fatalf("server received %d messages, want 1", len(srv.messages))
	}

	if got := srv.messages[0].authUser; got != "user@example.com" {
		t.Errorf("authenticated user = %q, want user@example.com", got)
	}

	if got := srv.messages[0].to; len(got) != 2 {
		t.Errorf("RCPT TO = %v, want 2 recipients", got)
	}
}

func TestSendMailConnectionRefused(t *testing.T) {
	t.Parallel()

	// An address nothing is listening on: exercises SendMail's dial-failure
	// path without needing a real unreachable host.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error: %v", err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort() error: %v", err)
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi() error: %v", err)
	}

	cfg := SMTPSettings{Host: host, Port: port, Security: SMTPSecurityNone}

	if err := SendMail(context.Background(), cfg, "from@example.com", []string{"to@example.com"}, "s", "b"); err == nil {
		t.Fatal("SendMail() error = nil, want a connection error")
	}
}

func TestPostWebhookDefaults(t *testing.T) {
	t.Parallel()

	var gotMethod, gotContentType, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")

		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := Webhook{URL: srv.URL, Method: http.MethodPost}

	resp, err := PostWebhook(context.Background(), wh, []byte(`{"a":1}`), "application/json")
	if err != nil {
		t.Fatalf("PostWebhook() error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}

	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json default", gotContentType)
	}

	if gotBody != `{"a":1}` {
		t.Errorf("body = %q, want the given payload", gotBody)
	}
}

func TestPostWebhookCustomHeaders(t *testing.T) {
	t.Parallel()

	var gotContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := Webhook{URL: srv.URL, Method: http.MethodPost, Headers: map[string]string{"Content-Type": "text/plain"}}

	resp, err := PostWebhook(context.Background(), wh, []byte("hi"), "application/json")
	if err != nil {
		t.Fatalf("PostWebhook() error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if gotContentType != "text/plain" {
		t.Errorf("content-type = %q, want the header override to win over the default", gotContentType)
	}
}
