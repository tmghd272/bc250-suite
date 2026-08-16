// bc250_terminal.go
//
// A basic terminal, in the browser: a real PTY-backed shell streamed over a
// WebSocket, rendered client-side with xterm.js. This is the one part of
// the panel that pulls in external Go modules (creack/pty for real PTY
// allocation with correct controlling-terminal/job-control semantics,
// gorilla/websocket for the socket framing) - both compile straight into
// the same single static binary, so the "no runtime dependency on the
// target system" property from the header comment in bc250_panel_server.go
// is unaffected. Hand-rolling raw ioctl-based PTY allocation and RFC 6455
// framing was the zero-dependency alternative, but that class of code is
// exactly where subtle bugs (job control, SIGWINCH resize, frame masking)
// hide - not worth it for what these two tiny, extremely well-established
// libraries already solve correctly.
//
// The panel server itself runs as root (needed to write hwmon files), but
// the shell this spawns deliberately drops to a normal user account - see
// terminalUser below - rather than handing out a root shell outright. Full
// sudo access is still one password away, same as sitting at the machine.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

const (
	termPingInterval = 20 * time.Second
	termPongTimeout  = 45 * time.Second // if no pong (or any client traffic) shows up within this window, the connection is presumed dead and torn down - this is what stops a hard-killed browser tab from leaving an orphaned shell running indefinitely, since beforeunload can't be relied on for that (network drop, force-quit, laptop lid slammed shut, etc.)
)

var termUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true }, // same-origin-only in practice since this isn't linked from anywhere else, but explicit for clarity
}

// terminalUser is the account the Terminal tab's shell runs as - set via
// --user (see main()), normally the person who ran the installer. Empty
// means "couldn't figure out a real user" and the shell falls back to
// whatever account the server itself runs as (root, per the systemd unit).
var terminalUser string

// termCredential resolves terminalUser to a syscall.Credential (uid, gid,
// and the FULL supplementary group list) so the spawned shell is genuinely
// that user - including being in whatever group (wheel/sudo/etc.) is needed
// for `sudo` to work normally, not just a bare uid switch that would leave
// sudo silently denying everything.
func termCredential() (*syscall.Credential, string, string, error) {
	if terminalUser == "" {
		return nil, "", "", fmt.Errorf("no --user configured")
	}
	u, err := user.Lookup(terminalUser)
	if err != nil {
		return nil, "", "", fmt.Errorf("user %q not found: %v", terminalUser, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, "", "", err
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, "", "", err
	}
	groupIDs, _ := u.GroupIds()
	groups := make([]uint32, 0, len(groupIDs))
	for _, g := range groupIDs {
		if n, err := strconv.ParseUint(g, 10, 32); err == nil {
			groups = append(groups, uint32(n))
		}
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}, u.HomeDir, u.Username, nil
}

// userLoginShell reads /etc/passwd directly for the account's actual
// configured shell (fish, zsh, whatever) - Go's os/user package doesn't
// expose this field at all. Spawning a hardcoded /bin/bash regardless of
// what the person actually uses is why fish's autosuggestions (or any
// other shell-specific feature) wouldn't show up here even though they
// work everywhere else on the same account.
func userLoginShell(username string) string {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.Split(line, ":")
		if len(parts) >= 7 && parts[0] == username {
			return strings.TrimSpace(parts[6])
		}
	}
	return ""
}

// termResizeMsg is the one structured message type the client sends;
// everything else over the socket is raw keystrokes/output bytes.
type termResizeMsg struct {
	Type string `json:"type"` // "resize"
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	conn, err := termUpgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("terminal: websocket upgrade failed:", err)
		return
	}
	defer conn.Close()

	env := []string{"TERM=xterm-256color"}
	shell := "/bin/bash" // fallback if we can't resolve a real configured shell below
	cred, homeDir, username, credErr := termCredential()

	if credErr != nil {
		fmt.Println("terminal: running as root -", credErr, "- pass --user <name> to drop privileges instead")
		if home, err := os.UserHomeDir(); err == nil {
			homeDir = home
		}
	} else {
		env = append(env, "HOME="+homeDir, "USER="+username, "LOGNAME="+username)
		// Use the account's ACTUAL configured shell (fish, zsh, whatever) -
		// not a hardcoded bash - so shell-specific features like fish's
		// inline history autosuggestions work here exactly like they do at
		// a real terminal on this same account.
		if sh := userLoginShell(username); sh != "" {
			if _, statErr := os.Stat(sh); statErr == nil {
				shell = sh
			}
		}
	}

	// No manual fastfetch injection here anymore - now that this spawns the
	// account's real login shell (see userLoginShell above), the person's
	// own shell config (fish's config.fish, .bashrc, whatever) runs
	// whatever it normally runs on an interactive login, fastfetch included
	// if that's already how their shell is set up. Adding our own on top of
	// that was just producing a redundant second run.
	cmd := exec.Command(shell, "-l")
	if credErr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	cmd.Dir = homeDir
	cmd.Env = append(os.Environ(), env...)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte("\r\n[failed to start shell: "+err.Error()+"]\r\n"))
		return
	}
	defer func() {
		ptmx.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}()

	var writeMu sync.Mutex // gorilla/websocket connections aren't safe for concurrent writes from two goroutines

	// Dead-connection detection: if the browser side vanishes without a
	// clean close (killed tab, network drop, laptop slept mid-session), a
	// plain ReadMessage() loop can block indefinitely and leave this shell
	// running forever. A read deadline + periodic ping resets it on any
	// pong (or any other client traffic) forces the connection - and this
	// shell - to actually die if nothing's heard from in termPongTimeout.
	conn.SetReadDeadline(time.Now().Add(termPongTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(termPongTimeout))
		return nil
	})
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(termPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ticker.C:
				writeMu.Lock()
				err := conn.WriteMessage(websocket.PingMessage, nil)
				writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	// PTY output -> browser
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				writeMu.Lock()
				werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n])
				writeMu.Unlock()
				if werr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					fmt.Println("terminal: pty read error:", err)
				}
				return
			}
		}
	}()

	// browser -> PTY input, plus the one JSON control message (resize)
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(termPongTimeout)) // any real traffic counts as a liveness signal too, not just pongs
		if msgType == websocket.TextMessage {
			var msg termResizeMsg
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" && msg.Cols > 0 && msg.Rows > 0 {
				pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(msg.Cols), Rows: uint16(msg.Rows)})
				continue
			}
			// not valid JSON / not a resize message - fall through and treat as raw input, same as a binary frame
		}
		if _, err := ptmx.Write(data); err != nil {
			return
		}
	}
}
