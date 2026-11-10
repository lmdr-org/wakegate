package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionIDs(t *testing.T) {
	// systemd 252, then 256 (more columns); the first column is the id in both.
	out := "     44 1000 lmdr   pts/0\n     52 1000 lmdr\n" +
		"c1 120 gdm seat0 1234 greeter tty1 no -\n"
	got := strings.Join(sessionIDs([]byte(out)), ",")
	if got != "44,52,c1" {
		t.Fatalf("ids: %s", got)
	}
	if ids := sessionIDs(nil); len(ids) != 0 {
		t.Fatalf("no sessions: %v", ids)
	}
}

func TestParseSessions(t *testing.T) {
	out := `Id=44
Name=lmdr
Remote=yes
Type=tty
TTY=pts/0
Class=user
IdleHint=yes

Id=c1
Name=gdm
Remote=no
Type=wayland
Class=greeter
IdleHint=no

Id=3
Name=alice
Remote=no
Type=wayland
Class=user
IdleHint=no
`
	s := parseSessions([]byte(out))
	if len(s) != 2 {
		t.Fatalf("sessions: %+v", s)
	}
	if s[0].Describe() != "lmdr (SSH, pts/0)" || !s[0].Idle || s[1].Describe() != "alice (desktop)" || s[1].Idle {
		t.Fatalf("sessions: %+v", s)
	}
}

func TestParseInhibitors(t *testing.T) {
	out := `{"type":"a(ssssuu)","data":[[["sleep","restic","Backup to the NAS","block",1000,4242],` +
		`["shutdown:sleep","NetworkManager","NetworkManager needs to turn off networks","delay",0,812]]]}`
	locks, err := parseInhibitors([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 || locks[0].Who != "restic" || locks[0].PID != 4242 || !locks[0].Blocks("suspend") ||
		locks[0].Blocks("poweroff") || locks[1].Blocks("suspend") {
		t.Fatalf("locks: %+v", locks)
	}
	if locks, err := parseInhibitors([]byte(`{"type":"a(ssssuu)","data":[[]]}`)); err != nil || len(locks) != 0 {
		t.Fatalf("none: %+v %v", locks, err)
	}
	if _, err := parseInhibitors([]byte(`{"data":[[["sleep"]]]}`)); err == nil {
		t.Fatal("a short row must be refused")
	}
}

func TestParseMeminfo(t *testing.T) {
	m, err := parseMeminfo(strings.NewReader("MemTotal:       32768000 kB\nMemFree: 1 kB\nMemAvailable:   16384000 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 32768000*1024 || m.Available != 16384000*1024 {
		t.Fatalf("%+v", m)
	}
	if _, err := parseMeminfo(strings.NewReader("MemFree: 1 kB\n")); err == nil {
		t.Fatal("missing fields must be an error")
	}
}

func TestBusyCommands(t *testing.T) {
	ctx := context.Background()
	var l Linux
	if busy, err := l.Busy(ctx, "true"); !busy || err != nil {
		t.Fatalf("true: %v %v", busy, err)
	}
	if busy, err := l.Busy(ctx, "exit 1"); busy || err != nil {
		t.Fatalf("exit 1: %v %v", busy, err)
	}
	if _, err := l.Busy(ctx, "no-such-command-wakegate"); err == nil || !strings.Contains(err.Error(), "127") {
		t.Fatalf("missing command: %v", err)
	}
}

func TestWaitResume(t *testing.T) {
	dir := t.TempDir()
	write := func(success, fail string) {
		_ = os.WriteFile(filepath.Join(dir, "success"), []byte(success+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "fail"), []byte(fail+"\n"), 0o644)
	}
	write("3", "0")
	done := make(chan error, 1)
	go func() { done <- waitResume(context.Background(), dir, 5*time.Millisecond, time.Second) }()
	time.Sleep(20 * time.Millisecond)
	write("4", "0")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	go func() { done <- waitResume(context.Background(), dir, 5*time.Millisecond, time.Second) }()
	time.Sleep(20 * time.Millisecond)
	write("4", "1")
	if err := <-done; err != ErrSleepFailed {
		t.Fatalf("failed suspend: %v", err)
	}
	if err := waitResume(context.Background(), dir, 5*time.Millisecond, 30*time.Millisecond); err == nil {
		t.Fatal("no resume must time out")
	}
}

func TestSlept(t *testing.T) {
	if Slept(10*time.Second, 9*time.Second) || !Slept(2*time.Hour, 15*time.Second) {
		t.Fatal("Slept")
	}
}
