//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/windows"

	"termhub/internal/proto"
)

// Probe connects to the Hub once and reports precisely what is wrong, so that
// enrolment either works or leaves nothing half-installed (docs/M5 第 3 节).
func Probe(ctx context.Context, cfg Config) error {
	url := "wss" + strings.TrimPrefix(cfg.HubURL, "https") + "/node/link"
	if !strings.HasPrefix(cfg.HubURL, "https://") {
		return errors.New("the Hub address must start with https://")
	}
	d := websocket.Dialer{TLSClientConfig: cfg.TLS, HandshakeTimeout: 10 * time.Second, Proxy: nil}
	ws, resp, err := d.DialContext(ctx, url, http.Header{"X-TH-Node-Token": {cfg.Token}})
	if err != nil {
		switch {
		case resp != nil && resp.StatusCode == http.StatusUnauthorized:
			return errors.New("the Hub refused the node token: it is wrong, already replaced, or the node is disabled")
		case resp != nil:
			return fmt.Errorf("the Hub answered %s: is this really a termhub address?", resp.Status)
		case strings.Contains(err.Error(), "does not match the enrolled pin"):
			return err
		case strings.Contains(err.Error(), "certificate"):
			return fmt.Errorf("the Hub's certificate is not trusted (%v). For a Hub with a self-signed certificate pass --pin with the fingerprint shown on the Hub's system page", err)
		default:
			return fmt.Errorf("cannot reach the Hub at %s: %v", cfg.HubURL, err)
		}
	}
	defer ws.Close()
	name, _ := os.Hostname()
	hello, _ := proto.EncodeMsg(&proto.Msg{T: proto.MsgHello, ID: 1, Proto: proto.Current.String(), AgentVer: Version, Name: name, Fingerprint: Fingerprint()})
	if err := ws.WriteMessage(websocket.TextMessage, hello); err != nil {
		return err
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := ws.ReadMessage()
	if err != nil {
		return fmt.Errorf("the Hub closed the connection during the handshake: %v", err)
	}
	m, err := proto.DecodeMsg(raw)
	if err != nil {
		return err
	}
	if m.T != proto.MsgWelcome {
		return fmt.Errorf("the Hub refused this machine: %s %s", m.Code, m.Text)
	}
	return nil
}

// StartHostProcess launches "<this exe> host" detached from the agent: no
// console, no window, its own process group, and outside the agent's Job when
// the Job allows it, so that the agent's end is never the host's (docs/M3 第 3 节).
func StartHostProcess(pipe string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"host"}
	if pipe != "" {
		args = append(args, "-pipe", pipe)
	}
	for _, flags := range []uint32{
		windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_BREAKAWAY_FROM_JOB,
		windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, // the Job forbids breaking away
	} {
		cmd := exec.Command(self, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err = cmd.Start(); err == nil {
			cmd.Process.Release()
			return nil
		}
	}
	return err
}
