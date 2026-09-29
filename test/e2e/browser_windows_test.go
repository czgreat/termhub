//go:build windows

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBrowser runs web/tests through Playwright against a real Hub with the
// front end embedded, and a real agent the test itself enrols. Set
// TH_SKIP_WEB_BUILD=1 when web/ was just built.
func TestBrowser(t *testing.T) {
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	web := filepath.Join(root, "web")
	if _, err := os.Stat(filepath.Join(web, "node_modules")); err != nil {
		t.Skip("web/node_modules missing: run npm install in web/ first")
	}
	if os.Getenv("TH_SKIP_WEB_BUILD") == "" {
		c := exec.Command("npm.cmd", "run", "build")
		c.Dir = web
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("npm run build: %v\n%s", err, out)
		}
	}
	dir := t.TempDir()
	hubExe := build(t, dir, "termhub.exe", "termhub/cmd/termhub")
	agentExe := build(t, dir, "termhub-agent.exe", "termhub/cmd/termhub-agent")
	t.Cleanup(func() { killByPath(agentExe); killByPath(hubExe); time.Sleep(500 * time.Millisecond) })

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	base := "https://" + addr
	hub := exec.Command(hubExe)
	hub.Env = append(testEnvironment(), "TH_DATA_DIR="+filepath.Join(dir, "data"), "TH_PUBLIC_URL="+base, "TH_LAN_LISTEN="+addr)
	stdout, _ := hub.StdoutPipe()
	if err := hub.Start(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	info := map[string]string{}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var line map[string]any
			if json.Unmarshal(sc.Bytes(), &line) == nil {
				mu.Lock()
				for _, k := range []string{"setup_token", "cert_fingerprint"} {
					if v, ok := line[k].(string); ok {
						info[k] = v
					}
				}
				mu.Unlock()
			}
		}
	}()
	get := func(k string) string { mu.Lock(); defer mu.Unlock(); return info[k] }
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end) && (get("setup_token") == "" || get("cert_fingerprint") == ""); {
		time.Sleep(100 * time.Millisecond)
	}
	if get("setup_token") == "" {
		t.Fatal("the Hub did not print its setup token")
	}

	pw := exec.Command("npx.cmd", "playwright", "test", "--reporter=line")
	pw.Dir = web
	pw.Env = append(testEnvironment(), "TH_URL="+base, "TH_SETUP_TOKEN="+get("setup_token"), "TH_PIN="+get("cert_fingerprint"),
		"TH_AGENT_EXE="+agentExe, "TH_TESTCLI="+testcli, "CI=1")
	out, err := pw.CombinedOutput()
	if err != nil {
		t.Fatalf("playwright: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "passed") {
		t.Fatalf("unexpected playwright output:\n%s", out)
	}
	t.Logf("%s", lastLines(string(out), 5))
}

// TestWebUnit runs web/tests/unit (node:test, no browser): the page's input
// queue and history state against a fake Hub (问题单 1).
func TestWebUnit(t *testing.T) {
	web, _ := filepath.Abs(filepath.Join("..", "..", "web"))
	if _, err := os.Stat(filepath.Join(web, "node_modules")); err != nil {
		t.Skip("web/node_modules missing: run npm install in web/ first")
	}
	c := exec.Command("npm.cmd", "run", "test:unit")
	c.Dir = web
	out, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "fail 0") {
		t.Fatalf("npm run test:unit: %v\n%s", err, out)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return fmt.Sprint(strings.Join(lines, "\n"))
}
