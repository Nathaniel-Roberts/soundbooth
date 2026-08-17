package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// remoteCmdMsg arrives from `soundbooth trigger|stop|marker` via the
// control socket, so a Stream Deck key or shell alias can drive the TUI.
type remoteCmdMsg struct{ cmd string }

func controlSockPath() string { return filepath.Join(stateDir(), "control.sock") }

// startControl listens on the unix control socket and forwards one-line
// commands into the running program.
func startControl(p *tea.Program) (net.Listener, error) {
	path := controlSockPath()
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				cmd := strings.TrimSpace(line)
				if cmd != "" {
					p.Send(remoteCmdMsg{cmd})
				}
				_, _ = c.Write([]byte("ok\n"))
			}(conn)
		}
	}()
	return ln, nil
}

// sendControl delivers a command to a running soundbooth instance.
func sendControl(cmd string) error {
	conn, err := net.Dial("unix", controlSockPath())
	if err != nil {
		return fmt.Errorf("no running soundbooth instance (%v)", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return err
	}
	buf := make([]byte, 16)
	_, _ = conn.Read(buf)
	return nil
}
