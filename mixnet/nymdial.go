package mixnet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNymdialMissing means the nymdial binary was not found. Callers must not
// fall back to a direct connection.
var ErrNymdialMissing = errors.New("nymdial not found; refusing to fall back to a direct connection")

// Nymdial opens streams through the Nym mixnet with the nymdial binary
// (https://github.com/apeirs/nymdial), which runs Nym's mixnet client and a
// userspace TCP/IP stack in-process over an IP Packet Router exit. No
// nym-socks5-client, no local proxy port.
//
// Every stream is its own `nymdial connect HOST PORT --events` child process:
// a new ephemeral mixnet client (fresh Nym identity), its own exit from the
// nym-api (with rotation past exits that don't answer and a health check),
// and DNS over the mixnet (the hostname is passed unresolved; nymdial looks
// it up with DNS-over-HTTPS through its tunnel). The child's stdin and stdout
// are one end of a socketpair; the other end is the net.Conn returned by
// Open, so TLS and HTTP/2 run in this process, end to end with the server.
type Nymdial struct {
	Binary       string        // path to nymdial (see FindNymdial)
	Args         []string      // extra nymdial options for every stream, e.g. --country DE, --exit 2205
	SetupTimeout time.Duration // budget to get a stream open, exit failover included (default 3m)
	Trace        Tracer        // nymdial's own log lines go here
}

// ExitInfo is the Nym exit gateway a stream left from: what the server sees.
type ExitInfo struct {
	NodeID   uint32   `json:"node_id"`
	Country  string   `json:"country"`
	IPs      []string `json:"ips"`
	Label    string   `json:"label"`
	Identity string   `json:"identity"`
	Version  string   `json:"version"`
}

// event is one JSON line from `nymdial connect --events`.
type event struct {
	Event    string   `json:"event"`
	Step     string   `json:"step"`
	Message  string   `json:"message"`
	Label    string   `json:"label"`
	OK       bool     `json:"ok"`
	Error    *string  `json:"error"`
	Elapsed  float64  `json:"elapsed_s"`
	Exit     ExitInfo `json:"exit"`
	TunnelS  float64  `json:"tunnel_s"`
	ConnectS float64  `json:"connect_s"`
}

// NymdialError is a stream nymdial could not open; Step says where it failed
// (exit, dns, tcp, tls, directory, timeout, exited).
type NymdialError struct {
	Step, Msg string
}

func (e *NymdialError) Error() string { return "nymdial: " + e.Msg }

// FindNymdial returns explicit, else $ZECMIX_NYMDIAL, else nymdial on PATH.
func FindNymdial(explicit string) (string, error) {
	for _, c := range []string{explicit, os.Getenv("ZECMIX_NYMDIAL")} {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c, nil
		}
		return "", fmt.Errorf("%w: %s is not an executable file", ErrNymdialMissing, c)
	}
	if p, err := exec.LookPath("nymdial"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("%w: build it from https://github.com/apeirs/nymdial (cargo install --git https://github.com/apeirs/nymdial), then put it on PATH, pass --nymdial PATH or set ZECMIX_NYMDIAL", ErrNymdialMissing)
}

func (n *Nymdial) trace(format string, args ...any) {
	if n.Trace != nil {
		n.Trace(format, args...)
	}
}

// NymConn is one open stream. Close also ends its nymdial process.
type NymConn struct {
	net.Conn
	Exit    ExitInfo
	Tunnel  time.Duration // mixnet client + exit + health check
	Connect time.Duration // DNS over the mixnet + TCP through the tunnel
	cmd     *exec.Cmd
	done    chan struct{}
	once    sync.Once
}

// Close closes the stream and stops the nymdial process behind it.
func (c *NymConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { stop(c.cmd, c.done) })
	return err
}

// stop asks a still-running nymdial to exit and reaps it in the background.
func stop(cmd *exec.Cmd, done chan struct{}) {
	select {
	case <-done:
		return
	default:
	}
	_ = cmd.Process.Signal(os.Interrupt)
	go func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	}()
}

// Open starts `nymdial connect host port --events` and returns the stream once
// nymdial reports it open.
func (n *Nymdial) Open(ctx context.Context, host string, port int) (*NymConn, error) {
	if n.Binary == "" {
		return nil, fmt.Errorf("%w: no binary configured", ErrNymdialMissing)
	}
	ours, theirs, err := socketpair()
	if err != nil {
		return nil, fmt.Errorf("nymdial: socketpair: %w", err)
	}
	args := append(append([]string{}, n.Args...), "connect", host, strconv.Itoa(port), "--events")
	cmd := exec.Command(n.Binary, args...)
	cmd.Stdin, cmd.Stdout = theirs, theirs
	stderr, err := cmd.StderrPipe()
	if err != nil {
		ours.Close()
		theirs.Close()
		return nil, err
	}
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		ours.Close()
		theirs.Close()
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: %v", ErrNymdialMissing, err)
		}
		return nil, fmt.Errorf("nymdial: start %s: %w", n.Binary, err)
	}
	theirs.Close()
	done := make(chan struct{})
	events := make(chan event, 16)
	tag := fmt.Sprintf("nymdial[%d]", cmd.Process.Pid)
	go func() {
		readEvents(stderr, events, func(line string) { n.trace("%s %s", tag, line) })
		_ = cmd.Wait()
		close(done)
	}()
	conn, err := net.FileConn(ours)
	ours.Close()
	if err != nil {
		stop(cmd, done)
		return nil, fmt.Errorf("nymdial: %w", err)
	}
	fail := func(e error) (*NymConn, error) {
		conn.Close()
		stop(cmd, done)
		return nil, e
	}
	budget := n.SetupTimeout
	if budget <= 0 {
		budget = 3 * time.Minute
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-timer.C:
			return fail(&NymdialError{"timeout", fmt.Sprintf("%s:%d: no stream through the mixnet within %s", host, port, budget)})
		case ev, ok := <-events:
			if !ok {
				<-done
				return fail(&NymdialError{"exited", fmt.Sprintf("%s:%d: nymdial exited (%v) before the stream opened", host, port, cmd.ProcessState)})
			}
			switch ev.Event {
			case "error":
				return fail(&NymdialError{ev.Step, fmt.Sprintf("%s:%d: %s", host, port, ev.Message)})
			case "connected":
				c := &NymConn{Conn: conn, Exit: ev.Exit, Tunnel: secs(ev.TunnelS), Connect: secs(ev.ConnectS), cmd: cmd, done: done}
				n.trace("nym  %s:%d open via exit %s [%s] (fresh mixnet client; tunnel %.2fs, DNS+TCP %.2fs, %.2fs in all)",
					host, port, c.Exit.Label, strings.Join(c.Exit.IPs, ", "), ev.TunnelS, ev.ConnectS, time.Since(t0).Seconds())
				return c, nil
			}
		}
	}
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// readEvents turns JSON lines into events and passes every other line to logf.
// It closes events when nymdial's stderr ends.
func readEvents(r io.Reader, events chan<- event, logf func(string)) {
	defer close(events)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "{") {
			var ev event
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Event != "" {
				// Don't block nymdial on a full channel once nobody is waiting for setup events.
				select {
				case events <- ev:
				default:
				}
				continue
			}
		}
		if line != "" {
			logf(line)
		}
	}
	// Drain stderr to the end so nymdial never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, r)
}
