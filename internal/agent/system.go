package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/clauer-org/wakegate/internal/api"
)

// System is what the agent needs from the machine. Linux implements it; tests replace it.
type System interface {
	// Sleep suspends, hibernates or powers off. For suspend and hibernate it may return
	// before the machine is asleep: WaitResume follows.
	Sleep(ctx context.Context, action string) error
	// WaitResume returns once the machine is back from sleep.
	WaitResume(ctx context.Context, timeout time.Duration) error
	Sessions(ctx context.Context) ([]Session, error)
	Inhibitors(ctx context.Context) ([]Inhibitor, error)
	// Busy runs a keep_awake command: true if it exits 0. An error means the check
	// itself failed (not found, timed out).
	Busy(ctx context.Context, command string) (bool, error)
	Memory() (*api.Memory, error)
}

// Session is a logind session of class "user": someone logged in.
type Session struct {
	ID     string
	User   string
	Type   string // tty, x11, wayland, mir, unspecified
	TTY    string // pts/0; empty for a session without a terminal (ssh host command)
	Remote bool
	Idle   bool // logind's IdleHint
}

// Describe names the session for people: "lmdr (SSH, pts/0)".
func (s Session) Describe() string {
	where := "console"
	switch {
	case s.Remote:
		where = "SSH"
	case s.Type == "x11" || s.Type == "wayland" || s.Type == "mir":
		where = "desktop"
	}
	if s.TTY != "" {
		where += ", " + s.TTY
	}
	return fmt.Sprintf("%s (%s)", s.User, where)
}

// Inhibitor is a systemd inhibitor lock.
type Inhibitor struct {
	What string // colon-separated: sleep, shutdown, idle, handle-lid-switch...
	Who  string
	Why  string
	Mode string // block or delay
	UID  uint32
	PID  uint32
}

// Blocks reports whether the lock prevents action.
func (i Inhibitor) Blocks(action string) bool {
	if i.Mode != "block" {
		return false
	}
	want := "sleep"
	if action == api.PowerOff {
		want = "shutdown"
	}
	for _, w := range strings.Split(i.What, ":") {
		if w == want {
			return true
		}
	}
	return false
}

// Linux is System on a machine with systemd. Sleeping as a user other than root needs
// polkit to allow the org.freedesktop.login1 actions (the installer adds a rule).
type Linux struct{}

// CommandTimeout bounds each keep_awake command.
const CommandTimeout = 10 * time.Second

func (Linux) Sleep(ctx context.Context, action string) error {
	verb := map[string]string{api.Suspend: "suspend", api.Hibernate: "hibernate", api.PowerOff: "poweroff"}[action]
	if verb == "" {
		return fmt.Errorf("unknown sleep action %q", action)
	}
	out, err := exec.CommandContext(ctx, "systemctl", verb).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (Linux) WaitResume(ctx context.Context, timeout time.Duration) error {
	return waitResume(ctx, "/sys/power/suspend_stats", 500*time.Millisecond, timeout)
}

func (Linux) Sessions(ctx context.Context) ([]Session, error) {
	out, err := exec.CommandContext(ctx, "loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return nil, fmt.Errorf("loginctl list-sessions: %w", err)
	}
	ids := sessionIDs(out)
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"show-session"}, ids...)
	args = append(args, "-p", "Id", "-p", "Name", "-p", "Class", "-p", "Type", "-p", "TTY", "-p", "Remote", "-p", "IdleHint")
	out, err = exec.CommandContext(ctx, "loginctl", args...).Output()
	if err != nil {
		// A session that closed between the two calls makes show-session fail.
		return nil, fmt.Errorf("loginctl show-session: %w", err)
	}
	return parseSessions(out), nil
}

// sessionIDs reads the first column of `loginctl list-sessions --no-legend`, the same in
// every systemd version.
func sessionIDs(out []byte) []string {
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.ContainsAny(f[0], "0123456789") {
			continue
		}
		ids = append(ids, f[0])
	}
	return ids
}

// parseSessions reads `loginctl show-session` output (blocks of key=value separated by
// blank lines) and keeps sessions of class user.
func parseSessions(out []byte) []Session {
	var sessions []Session
	for _, block := range strings.Split(strings.TrimSpace(string(out)), "\n\n") {
		props := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				props[k] = v
			}
		}
		if props["Id"] == "" || !strings.HasPrefix(props["Class"], "user") {
			continue
		}
		sessions = append(sessions, Session{ID: props["Id"], User: props["Name"], Type: props["Type"],
			TTY: props["TTY"], Remote: props["Remote"] == "yes", Idle: props["IdleHint"] == "yes"})
	}
	return sessions
}

func (Linux) Inhibitors(ctx context.Context) ([]Inhibitor, error) {
	out, err := exec.CommandContext(ctx, "busctl", "call", "--json=short", "org.freedesktop.login1",
		"/org/freedesktop/login1", "org.freedesktop.login1.Manager", "ListInhibitors").Output()
	if err != nil {
		return nil, fmt.Errorf("busctl ListInhibitors: %w", err)
	}
	return parseInhibitors(out)
}

// parseInhibitors reads busctl's JSON for ListInhibitors, of D-Bus type a(ssssuu): what,
// who, why, mode, uid, pid.
func parseInhibitors(out []byte) ([]Inhibitor, error) {
	var reply struct {
		Data [][][]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return nil, fmt.Errorf("ListInhibitors: %w", err)
	}
	if len(reply.Data) == 0 {
		return nil, nil
	}
	var locks []Inhibitor
	for _, row := range reply.Data[0] {
		if len(row) != 6 {
			return nil, fmt.Errorf("ListInhibitors: %d fields, want 6", len(row))
		}
		var l Inhibitor
		for i, dst := range []any{&l.What, &l.Who, &l.Why, &l.Mode, &l.UID, &l.PID} {
			if err := json.Unmarshal(row[i], dst); err != nil {
				return nil, fmt.Errorf("ListInhibitors: %w", err)
			}
		}
		locks = append(locks, l)
	}
	return locks, nil
}

func (Linux) Busy(ctx context.Context, command string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 512}
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, fmt.Errorf("timed out after %s", CommandTimeout)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch code := exit.ExitCode(); code {
		case 126, 127: // not executable, not found: the check is broken, not "not busy"
			return false, fmt.Errorf("exit status %d: %s", code, strings.TrimSpace(stderr.String()))
		default:
			return false, nil
		}
	}
	return false, err
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.n -= k
		_, _ = l.w.Write(p[:k])
	}
	return len(p), nil
}

func (Linux) Memory() (*api.Memory, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMeminfo(f)
}

// parseMeminfo reads MemTotal and MemAvailable from /proc/meminfo.
func parseMeminfo(r io.Reader) (*api.Memory, error) {
	var m api.Memory
	fields := map[string]*uint64{"MemTotal": &m.Total, "MemAvailable": &m.Available}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		dst := fields[key]
		if !ok || dst == nil {
			continue
		}
		kb, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("meminfo: %s: %q", key, rest)
		}
		*dst = kb * 1024
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if m.Total == 0 || m.Available > m.Total {
		return nil, errors.New("meminfo: MemTotal or MemAvailable missing")
	}
	return &m, nil
}

// bootTime reads the boot time from /proc/stat (btime).
func bootTime() (time.Time, error) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(s, 0), nil
		}
	}
	return time.Time{}, errors.New("/proc/stat: no btime")
}

// ErrSleepFailed: the kernel counted a failed suspend.
var ErrSleepFailed = errors.New("suspend failed (suspend_stats/fail increased)")

// waitResume returns when the kernel's suspend counters (dir is /sys/power/suspend_stats)
// show a successful or failed suspend, or when the wall clock jumps ahead of the
// monotonic clock, which stops while the machine sleeps (hibernation does not update the
// counters).
func waitResume(ctx context.Context, dir string, poll, timeout time.Duration) error {
	success0, fail0, statsErr := readSuspendStats(dir)
	start := time.Now()
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		if statsErr == nil {
			if success, fail, err := readSuspendStats(dir); err == nil {
				if success > success0 {
					return nil
				}
				if fail > fail0 {
					return ErrSleepFailed
				}
			}
		}
		now := time.Now()
		if Slept(now.Round(0).Sub(start.Round(0)), now.Sub(start)) {
			return nil
		}
		if now.Sub(start) > timeout {
			return fmt.Errorf("still awake %s after the sleep request", timeout)
		}
	}
}

// Slept reports whether the machine slept: over the same period, the wall clock advanced
// clearly more than the monotonic one.
func Slept(wall, mono time.Duration) bool {
	return wall-mono > 5*time.Second
}

func readSuspendStats(dir string) (success, fail int, err error) {
	read := func(name string) (int, error) {
		raw, err := os.ReadFile(dir + "/" + name)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if success, err = read("success"); err != nil {
		return 0, 0, err
	}
	if fail, err = read("fail"); err != nil {
		return 0, 0, err
	}
	return success, fail, nil
}
