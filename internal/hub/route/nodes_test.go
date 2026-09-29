package route

import (
	"testing"
	"time"
)

// docs/M7 验收 11: a node that still runs sessions cannot be deleted, because
// its sessions would live on, on a machine that can never connect again.
func TestDeleteNodeWithSessions(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, pid := f.nodeWithProfile(admin, "pc1")
	node, _, _ := f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)

	_, out := admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid})
	sid := out["session"].(map[string]any)["sid"].(string)

	if code, out := admin.call("DELETE", "/api/admin/nodes/1", nil); code != 409 || out["code"] != "has_sessions" {
		t.Fatalf("deleting a node with a running session: %d %v", code, out)
	}
	if !f.hub.Online(1) {
		t.Fatal("a refused deletion must not disconnect the node")
	}

	// End the session, then the node may go; its connection is cut and its token is dead.
	if code, _ := admin.call("DELETE", "/api/sessions/"+sid, nil); code != 200 {
		t.Fatalf("close: %d", code)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if row, _ := f.reg.session(sid); row.EndedAt != 0 {
			break
		}
	}
	if code, out := admin.call("DELETE", "/api/admin/nodes/1", nil); code != 200 {
		t.Fatalf("deleting the idle node: %d %v", code, out)
	}
	select {
	case <-node.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the deleted node's connection stayed open")
	}
	if _, resp, err := f.dialNode(token, "fp", nil); err == nil || resp.StatusCode != 401 {
		t.Fatal("a deleted node's token must stop working")
	}
	// Its profiles went with it; the session register keeps the history.
	var profiles, sessions int
	f.db.QueryRow(`SELECT COUNT(*) FROM cli_profiles WHERE node_id=1`).Scan(&profiles)
	f.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE node_id=1`).Scan(&sessions)
	if profiles != 0 || sessions != 1 {
		t.Fatalf("after deletion: %d profiles, %d session records", profiles, sessions)
	}
	// Only a reverified administrator may do any of this.
	plain := f.login("root", pw, false)
	f.db.Exec(`UPDATE login_sessions SET reverified_until=0`)
	if code, out := plain.call("POST", "/api/admin/nodes", map[string]string{"name": "pc2"}); code != 403 || out["code"] != "reverify_required" {
		t.Fatalf("node creation without reverification: %d %v", code, out)
	}
	// Wrong node tokens from one address fill no audit log: one line per window (第二轮复核).
	for i := 0; i < 5; i++ {
		f.dialNode("wrong-token", "fp", nil)
	}
	var lines int
	f.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE event='node_auth' AND result='failed'`).Scan(&lines)
	if lines != 1 {
		t.Fatalf("%d audit lines for five wrong tokens", lines)
	}
}

// Nodes, and so their profiles, are listed in the administrator's order.
func TestNodeOrder(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	f.nodeWithProfile(admin, "a-first")
	f.nodeWithProfile(admin, "b-second")
	order := func() (nodes, profiles []float64) {
		_, out := admin.call("GET", "/api/nodes", nil)
		for _, n := range out["nodes"].([]any) {
			nodes = append(nodes, n.(map[string]any)["id"].(float64))
		}
		_, out = admin.call("GET", "/api/profiles", nil)
		for _, p := range out["profiles"].([]any) {
			profiles = append(profiles, p.(map[string]any)["node_id"].(float64))
		}
		return
	}
	if n, p := order(); n[0] != 1 || p[0] != 1 {
		t.Fatalf("by name first: %v %v", n, p)
	}
	if code, out := admin.call("PATCH", "/api/admin/nodes/1", map[string]any{"position": 9}); code != 200 {
		t.Fatalf("move: %d %v", code, out)
	}
	if n, p := order(); n[0] != 2 || p[0] != 2 {
		t.Fatalf("after moving node 1 down: %v %v", n, p)
	}
	if code, _ := admin.call("PATCH", "/api/admin/nodes/1", map[string]any{}); code != 400 {
		t.Fatalf("an empty change must be refused: %d", code)
	}
}
