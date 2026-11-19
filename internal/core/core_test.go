package core

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// recordingRunner answers every call with answer and records the arguments.
func recordingRunner(answer string, calls *[][]string) Runner {
	return func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		return []byte(answer), nil
	}
}

// A prompt may start with a dash; without the separator hc would read it as
// one of its own options.
func TestSendPutsTheTextAfterTheSeparator(t *testing.T) {
	var calls [][]string
	client := NewWithRunner(recordingRunner(`{}`, &calls))
	if _, err := client.Send(context.Background(), "t1w", "--force yes", true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(context.Background(), "t1w", "hello", false); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"send", "--json", "--if", "idle", "--", "t1w", "--force yes"},
		{"send", "--json", "--", "t1w", "hello"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %q", calls)
	}
}

// Names and machine/t7 forms could reach a terminal nobody listed, or another
// machine; neither reaches hc.
func TestEveryCallRefusesAnythingButALocalAddress(t *testing.T) {
	var calls [][]string
	client := NewWithRunner(recordingRunner(`{}`, &calls))
	ctx := context.Background()
	for _, address := range []string{"", "review", "vps/t1", "t1w;id", "-t1", "T1W", "t123456789"} {
		if _, err := client.Get(ctx, address); err == nil {
			t.Fatalf("get %q accepted", address)
		}
		if _, err := client.Read(ctx, address, 5); err == nil {
			t.Fatalf("read %q accepted", address)
		}
		if _, err := client.Wait(ctx, address, time.Second); err == nil {
			t.Fatalf("wait %q accepted", address)
		}
		if _, err := client.Send(ctx, address, "x", true); err == nil {
			t.Fatalf("send %q accepted", address)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("reached hc: %q", calls)
	}
}

func TestReadAndWaitPassTheirLimits(t *testing.T) {
	var calls [][]string
	client := NewWithRunner(recordingRunner(`{"text":"x","complete":true}`, &calls))
	if _, err := client.Read(context.Background(), "t1w", -3); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Wait(context.Background(), "t1w", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"read", "--json", "--lines", "0", "--", "t1w"},
		{"wait", "--json", "--timeout", "1.500", "--", "t1w"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %q", calls)
	}
}

func fakeHC(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// hc reports its own errors as JSON on stderr with a non-zero exit, measured
// 2026-11-19: not_found exits 3, blocked 9, status_mismatch 10.
func TestHCErrorsKeepTheirKind(t *testing.T) {
	path := fakeHC(t, `echo '{"error":{"hint":"nothing was typed","kind":"blocked","message":"t1 is blocked"}}' >&2; exit 9`)
	_, err := newUnguarded(path).Send(context.Background(), "t1", "x", true)
	if Kind(err) != "blocked" || !strings.Contains(err.Error(), "nothing was typed") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnUnexplainedFailureIsAnError(t *testing.T) {
	path := fakeHC(t, `echo 'segfault' >&2; exit 139`)
	_, err := newUnguarded(path).Get(context.Background(), "t1")
	if err == nil || Kind(err) != "" || !strings.Contains(err.Error(), "segfault") {
		t.Fatalf("err = %v", err)
	}
}

// The bridge is not a terminal: no inherited HC_* variable reaches hc, and
// hc is pinned to the socket the guard checked.
func TestTheBridgeIsNoTerminal(t *testing.T) {
	t.Setenv("HC_TERMINAL_ID", "t9")
	t.Setenv("HC_SCOPE", "s1")
	t.Setenv("HC_SOCKET", "/elsewhere.sock")
	path := fakeHC(t, `printf '{"text":"%s %s %s","complete":true}' "${HC_TERMINAL_ID:-none}" "${HC_SCOPE:-none}" "$HC_SOCKET"`)
	screen, err := (&Client{run: execRunner(path, "/checked.sock")}).Read(context.Background(), "t1", 0)
	if err != nil || screen.Text != "none none /checked.sock" {
		t.Fatalf("screen = %+v, err = %v", screen, err)
	}
}

func TestAHangingHCIsCutOff(t *testing.T) {
	path := fakeHC(t, `exec sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := newUnguarded(path).Get(ctx, "t1"); err == nil || time.Since(started) > 5*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(started))
	}
}

func TestOutputBeyondTheCapIsRefused(t *testing.T) {
	var buffer limitedBuffer
	buffer.limit = 4
	_, _ = buffer.Write([]byte("abcdef"))
	if !buffer.overflow || buffer.String() != "abcd" {
		t.Fatalf("buffer = %q overflow %v", buffer.String(), buffer.overflow)
	}
}

// newUnguarded skips the core check, for tests of the hc call itself.
func newUnguarded(path string) *Client { return &Client{run: execRunner(path, "")} }

// Measured 2026-11-19: any hc command, even "server status", starts a new
// core when none runs. The bridge checks the socket first and starts nothing.
func TestNoCoreMeansNoCall(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hcs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "default.sock")
	called := false
	inner := func(context.Context, ...string) ([]byte, error) {
		called = true
		return []byte(`{"terminals":[]}`), nil
	}
	client := &Client{run: requireCore(socket, inner)}
	if _, err := client.List(context.Background()); err == nil || called || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v, called = %v", err, called)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	if _, err := client.List(context.Background()); err != nil || !called {
		t.Fatalf("err = %v, called = %v", err, called)
	}
}

func TestDefaultSocketFollowsHCSession(t *testing.T) {
	t.Setenv("HOME", "/Users/someone")
	t.Setenv("HC_SESSION", "")
	if socket, _ := DefaultSocket(); socket != "/Users/someone/.local/state/herdr-core/default/default.sock" {
		t.Fatalf("socket = %s", socket)
	}
	t.Setenv("HC_SESSION", "work")
	if socket, _ := DefaultSocket(); socket != "/Users/someone/.local/state/herdr-core/work/work.sock" {
		t.Fatalf("socket = %s", socket)
	}
	t.Setenv("HC_SESSION", "../x")
	if _, err := DefaultSocket(); err == nil {
		t.Fatal("a path was accepted as a session")
	}
}

// A prompt sent while the agent stays quiet is reported, not assumed to run.
func TestSendReadsWhatHCSaw(t *testing.T) {
	var calls [][]string
	client := NewWithRunner(recordingRunner(`{"status":"done","started":false,"turn":4}`, &calls))
	result, err := client.Send(context.Background(), "t1", "x", false)
	if err != nil || result.Status == nil || *result.Status != "done" || result.Started == nil || *result.Started {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

// At most maxCalls hc processes run at once; one more is refused instead of
// queueing without bound.
func TestCallsAreBounded(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, maxCalls)
	client := NewWithRunner(func(ctx context.Context, _ ...string) ([]byte, error) {
		started <- struct{}{}
		<-release
		return []byte(`{"terminals":[]}`), nil
	})
	for range maxCalls {
		go func() { _, _ = client.List(context.Background()) }()
	}
	for range maxCalls {
		<-started
	}
	_, err := client.List(context.Background())
	close(release)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
}

func TestOnlyClaudeInTheForegroundCounts(t *testing.T) {
	for argv, want := range map[string]bool{
		`["claude","--dangerously-skip-permissions"]`: true,
		`["/Users/someone/.local/bin/claude"]`:        true,
		`["/bin/zsh","-l"]`:                           false,
		`["bash"]`:                                    false,
		`[]`:                                          false,
	} {
		var terminal Terminal
		if err := json.Unmarshal([]byte(`{"foreground":{"pid":1,"name":"2.1.328","argv":`+argv+`}}`), &terminal); err != nil {
			t.Fatal(err)
		}
		if terminal.RunsClaude() != want {
			t.Fatalf("%s: RunsClaude = %v", argv, !want)
		}
	}
	if (Terminal{}).RunsClaude() {
		t.Fatal("no foreground counted as Claude")
	}
}
