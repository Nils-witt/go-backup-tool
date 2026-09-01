package pipeline

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/report"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// writeFile writes contents to path, failing the test on any error.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}
}

// TestReportScheduleNext exercises the report package's actual use of
// cron.Schedule (RunReportLoop calls Next(time.Now()) each iteration), as a
// sanity check that a daily "0 7 * * *" schedule behaves the way the report
// loop's log/tests assume: today's occurrence if it hasn't passed yet,
// tomorrow's otherwise — including the exact-match instant, so a report sent
// right at its scheduled time doesn't loop and fire again immediately.
func TestReportScheduleNext(t *testing.T) {
	t.Parallel()

	sched, err := cron.ParseStandard("0 7 * * *")
	if err != nil {
		t.Fatalf("cron.ParseStandard() error: %v", err)
	}

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"later today", time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC), time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC)},
		{"already passed rolls to tomorrow", time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC), time.Date(2026, 8, 29, 7, 0, 0, 0, time.UTC)},
		{"exact match rolls to tomorrow", time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC), time.Date(2026, 8, 29, 7, 0, 0, 0, time.UTC)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := sched.Next(tc.now); !got.Equal(tc.want) {
				t.Errorf("Schedule.Next(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestBuildReportSummarizesReceiverEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()

	db, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	end := time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC)
	start := end.Add(-24 * time.Hour)
	inWindow := end.Add(-time.Hour)
	outOfWindow := end.Add(-25 * time.Hour) // just outside the window

	events := []store.ReceiverEvent{
		{At: inWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "a1.gpg", Size: 100, Success: true},
		{At: inWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "a2.gpg", Size: 200, Success: true},
		{At: inWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "a3.gpg", Success: false, Error: "disk full"},
		{At: inWindow, ReceiverID: "recv-b", Kind: store.ReceiverEventDelete, Key: "b1.gpg", Success: true},
		{At: outOfWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "old.gpg", Size: 999, Success: true},
	}

	for _, ev := range events {
		if err := db.SaveReceiverEvent(ctx, ev); err != nil {
			t.Fatalf("SaveReceiverEvent() error: %v", err)
		}
	}

	rc := &config.RunConfig{Receivers: map[string]config.ResolvedReceiver{
		"recv-a": {ID: "recv-a"},
		"recv-b": {ID: "recv-b"},
		"recv-c": {ID: "recv-c"}, // no events at all in the window
	}}

	report := buildReport(ctx, rc, db, start, end, discardLogger)

	if len(report.receivers) != 3 {
		t.Fatalf("report.receivers = %+v, want 3 entries (one per configured receiver)", report.receivers)
	}

	byID := make(map[string]receiverReportLine, len(report.receivers))
	for _, r := range report.receivers {
		byID[r.id] = r
	}

	want := map[string]receiverReportLine{
		"recv-a": {id: "recv-a", filesReceived: 2, bytesReceived: 300, errors: 1},
		"recv-b": {id: "recv-b"}, // its only event is a delete: no receives, no errors
		"recv-c": {id: "recv-c"}, // no events at all
	}

	for id, want := range want {
		if got := byID[id]; got != want {
			t.Errorf("%s summary = %+v, want %+v", id, got, want)
		}
	}

	wantErrors := []store.ReceiverErrorEvent{{At: inWindow, ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "a3.gpg", Error: "disk full"}}
	if !reflect.DeepEqual(report.errors, wantErrors) {
		t.Errorf("report.errors = %+v, want %+v", report.errors, wantErrors)
	}
}

func TestBuildReportDetectsStaleReceiver(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	freshDir := t.TempDir()
	writeFile(t, filepath.Join(freshDir, "recent.gpg"), "a")

	staleDir := t.TempDir()
	staleFile := filepath.Join(staleDir, "old.gpg")
	writeFile(t, staleFile, "a")

	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staleFile, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes() error: %v", err)
	}

	neverDir := t.TempDir()

	rc := &config.RunConfig{Receivers: map[string]config.ResolvedReceiver{
		"fresh": {ID: "fresh", Path: freshDir, StaleAfter: time.Hour},
		"stale": {ID: "stale", Path: staleDir, StaleAfter: time.Hour},
		"never": {ID: "never", Path: neverDir, StaleAfter: time.Hour},
	}}

	now := time.Now()
	report := buildReport(ctx, rc, nil, now.Add(-24*time.Hour), now, discardLogger)

	if len(report.stale) != 1 || report.stale[0].id != "stale" {
		t.Errorf("report.stale = %+v, want only \"stale\" (fresh isn't stale, never has nothing to be stale)", report.stale)
	}
}

func TestRenderReportBody(t *testing.T) {
	t.Parallel()

	report := reportContent{
		start: time.Date(2026, 8, 27, 7, 0, 0, 0, time.UTC),
		end:   time.Date(2026, 8, 28, 7, 0, 0, 0, time.UTC),
		receivers: []receiverReportLine{
			{id: "recv-a", filesReceived: 3, bytesReceived: 1500, errors: 0},
		},
		errors: []store.ReceiverErrorEvent{
			{At: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC), ReceiverID: "recv-a", Kind: store.ReceiverEventReceive, Key: "x.gpg", Error: "boom"},
		},
		stale: []staleReceiverLine{
			{id: "recv-b", staleAfter: 6 * time.Hour, lastSeen: time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)},
		},
	}

	body := renderReportBody(report)

	for _, want := range []string{"recv-a", "3 file(s)", "recv-b", "boom", "stale-after: 6h0m0s"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderReportBody() = %q, want it to contain %q", body, want)
		}
	}
}

func TestRenderReportBodyEmpty(t *testing.T) {
	t.Parallel()

	body := renderReportBody(reportContent{})

	for _, want := range []string{"No receivers configured", "No receivers currently stale", "No errors recorded"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderReportBody() = %q, want it to contain %q", body, want)
		}
	}
}

// fakeSMTPMessage is one message a fakeSMTPServer accepted, captured for a
// test to assert against.
type fakeSMTPMessage struct {
	authUser string
	from     string
	to       []string
	data     string
}

// fakeSMTPServer is a minimal SMTP server (no TLS, no real delivery) driving
// enough of the protocol for net/smtp's client — and so sendMail — to
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

	cfg := report.SMTPSettings{Host: host, Port: port, Security: report.SMTPSecurityNone}

	err := sendMail(context.Background(), cfg, "from@example.com", []string{"to@example.com"}, "Test subject", "line one\nline two")
	if err != nil {
		t.Fatalf("sendMail() error: %v", err)
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

	cfg := report.SMTPSettings{Host: host, Port: port, Security: report.SMTPSecurityNone, Username: "user@example.com", Password: "hunter2"}

	if err := sendMail(context.Background(), cfg, "from@example.com", []string{"a@example.com", "b@example.com"}, "Subj", "body"); err != nil {
		t.Fatalf("sendMail() error: %v", err)
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

	// An address nothing is listening on: exercises sendMail's dial-failure
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

	cfg := report.SMTPSettings{Host: host, Port: port, Security: report.SMTPSecurityNone}

	if err := sendMail(context.Background(), cfg, "from@example.com", []string{"to@example.com"}, "s", "b"); err == nil {
		t.Fatal("sendMail() error = nil, want a connection error")
	}
}
