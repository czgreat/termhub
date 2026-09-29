//go:build windows

package e2e

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"termhub/internal/hub/route"
	"termhub/internal/proto"
)

func totpNow(secretText string) string {
	secret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secretText)
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(time.Now().Unix()/30))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1000000)
}

func build(t *testing.T, dir, name, pkg string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go.exe")
	if _, err := os.Stat(gobin); err != nil {
		gobin = "go"
	}
	if b, err := exec.Command(gobin, "build", "-o", out, pkg).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

// killByPath ends every process started from exe: the detached session host
// is nobody's child, so it has to be found by its image path.
func killByPath(exe string) {
	exec.Command("pwsh.exe", "-NoProfile", "-Command",
		fmt.Sprintf("Get-Process | Where-Object { $_.Path -eq '%s' } | Stop-Process -Force", exe)).Run()
}

// The real executables, the real first-login flow, a real enrolment with a
// pinned certificate, the agent starting its own detached session host.
// Nothing here touches this machine's real termhub configuration: the agent's
// directory and the host's pipe are redirected.
func TestRealBinaries(t *testing.T) {
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
	pin, setupToken := get("cert_fingerprint"), get("setup_token")
	if pin == "" || setupToken == "" {
		t.Fatal("the Hub did not print its certificate fingerprint and setup token")
	}

	// The container's health probe is the binary itself (the image has no shell).
	probe := exec.Command(hubExe, "healthcheck")
	probe.Env = hub.Env
	if b, err := probe.CombinedOutput(); err != nil || strings.TrimSpace(string(b)) != "ok" {
		t.Fatalf("healthcheck against a healthy Hub: %v %s", err, b)
	}
	probe = exec.Command(hubExe, "healthcheck")
	probe.Env = append(testEnvironment(), "TH_DATA_DIR="+filepath.Join(dir, "data"), "TH_LAN_LISTEN=127.0.0.1:1")
	if err := probe.Run(); err == nil {
		t.Fatal("healthcheck must fail when nothing answers")
	}

	tlsc := &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
		if hex.EncodeToString(sum[:]) != pin {
			return fmt.Errorf("certificate does not match the printed fingerprint")
		}
		return nil
	}}
	jar, _ := cookiejar.New(nil)
	w := &world{t: t, base: base, tlsc: tlsc, client: &http.Client{Jar: jar, Transport: &http.Transport{TLSClientConfig: tlsc}}}

	// First login, exactly as a person would do it.
	const pw = "correct horse battery"
	if code, out := w.call("POST", "/api/setup", map[string]string{"token": setupToken, "username": "root", "password": pw}); code != 200 {
		t.Fatalf("setup: %d %v", code, out)
	}
	if code, out := w.call("POST", "/api/auth/login", map[string]string{"username": "root", "password": pw}); code != 200 || out["done"] != true {
		t.Fatalf("login: %d %v", code, out)
	}
	_, me := w.call("GET", "/api/me", nil)
	w.csrf = me["csrf"].(string)
	_, begin := w.call("POST", "/api/me/totp/begin", map[string]string{})
	code, conf := w.call("POST", "/api/me/totp/confirm", map[string]string{"code": totpNow(begin["secret"].(string))})
	if code != 200 {
		t.Fatalf("totp confirm: %d %v", code, conf)
	}
	recovery := conf["recovery_codes"].([]any)[0].(string)
	if code, out := w.call("POST", "/api/auth/reverify", map[string]string{"code": recovery}); code != 200 {
		t.Fatalf("reverify with a recovery code: %d %v", code, out)
	}
	req, _ := http.NewRequest("GET", base, nil)
	for _, ck := range jar.Cookies(req.URL) {
		w.cookie = ck.Name + "=" + ck.Value
	}

	_, out := w.call("POST", "/api/admin/nodes", map[string]string{"name": "this-pc"})
	token := out["token"].(string)

	// Enrolment. The agent's directory is redirected into the test's temp dir.
	agentEnv := append(testEnvironment(), "LOCALAPPDATA="+filepath.Join(dir, "localappdata"))
	enroll := func(pinArg string) ([]byte, error) {
		c := exec.Command(agentExe, "enroll", "--hub", base, "--token", token, "--pin", pinArg)
		c.Env = agentEnv
		return c.CombinedOutput()
	}
	if b, err := enroll(strings.Repeat("0", 64)); err == nil || !strings.Contains(string(b), "does not match the enrolled pin") {
		t.Fatalf("enrolment with a wrong pin must fail and say why: %v\n%s", err, b)
	}
	cfgFile := filepath.Join(dir, "localappdata", "termhub", "agent.json")
	if _, err := os.Stat(cfgFile); err == nil {
		t.Fatal("a failed enrolment left a configuration behind")
	}
	if b, err := enroll(pin); err != nil {
		t.Fatalf("enrol: %v\n%s", err, b)
	}
	if raw, _ := os.ReadFile(cfgFile); len(raw) == 0 || strings.Contains(string(raw), token) {
		t.Fatalf("configuration missing or contains the token in the clear:\n%s", raw)
	}

	pipe := fmt.Sprintf(`\\.\pipe\termhub-bin-%d`, time.Now().UnixNano())
	run := exec.Command(agentExe, "run", "-pipe", pipe)
	run.Env = agentEnv
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	w.waitNode(true) // implies: config unsealed, pin accepted, host process spawned or not needed yet

	_, out = w.call("POST", "/api/admin/profiles", route.Profile{NodeID: 1, Name: "testcli", Mode: "direct", Command: testcli})
	pid := out["profile"].(map[string]any)["id"]
	code, out = w.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": dir})
	if code != 200 {
		t.Fatalf("create session through real binaries: %d %v", code, out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)
	v := w.view(sid)
	v.attach(false)
	v.waitText("READY")
	v.typeIn("echo 真实进程\r")
	v.waitText(`ECHO "真实进程"`)

	// The agent process is killed; the detached host keeps the session.
	run.Process.Kill()
	run.Wait()
	v.wait(proto.MsgNodeOffline)
	run2 := exec.Command(agentExe, "run", "-pipe", pipe)
	run2.Env = agentEnv
	if err := run2.Start(); err != nil {
		t.Fatal(err)
	}
	v.wait(proto.MsgNodeOnline)
	if a := v.attach(true); a.Mode != proto.ModeResume {
		t.Fatalf("the session should have survived the agent process: %+v", a)
	}
	v.typeIn("echo still-here\r")
	v.waitText(`ECHO "still-here"`)
	if code, _ := w.call("DELETE", "/api/sessions/"+sid+"?mode=force", nil); code != 200 {
		t.Fatalf("close: %d", code)
	}
	v.wait(proto.MsgExited)
}
