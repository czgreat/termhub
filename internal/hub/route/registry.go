// Package route implements docs/M7-节点与会话路由.md: nodes and their tokens,
// CLI profiles and bindings, permissions, the node link and the routing of
// terminal sessions between browsers and nodes.
package route

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"termhub/internal/hub/auth"
	"termhub/internal/hub/store"
	"termhub/internal/proto"
)

var (
	ErrForbidden   = &auth.Error{Code: "forbidden", Msg: "无权执行此操作", Status: 403}
	ErrNotFound    = &auth.Error{Code: "not_found", Msg: "对象不存在", Status: 404}
	ErrBadRequest  = &auth.Error{Code: "bad_request", Msg: "请求无效", Status: 400}
	ErrConflict    = &auth.Error{Code: "conflict", Msg: "名称已存在", Status: 409}
	ErrNodeOffline = &auth.Error{Code: proto.ErrNodeOffline, Msg: "节点离线", Status: 409}
	ErrBusy        = &auth.Error{Code: proto.ErrBusy, Msg: "会话数已达上限", Status: 409}
	ErrNotAttached = &auth.Error{Code: "not_attached", Msg: "请先打开这个会话，再接管", Status: 409}
	ErrReadOnly    = &auth.Error{Code: proto.ErrReadOnly, Msg: "只读：这个会话现在由别人操作", Status: 409}
	ErrHasSessions = &auth.Error{Code: "has_sessions", Msg: "该节点还有运行中的会话，请先结束它们", Status: 409}
)

// Node is a registered machine.
type Node struct {
	ID                  int64  `json:"id"`
	Name                string `json:"name"`
	Note                string `json:"note"`
	Status              string `json:"status"`
	Online              bool   `json:"online"`
	FingerprintMismatch bool   `json:"fingerprint_mismatch"`
	LastSeenAt          int64  `json:"last_seen_at"`
	AgentVer            string `json:"agent_ver"`
	HostVer             string `json:"host_ver"`
	OSBuild             string `json:"os_build"`
	PwshStore           bool   `json:"pwsh_store"`
	Position            int    `json:"position"`
}

// Profile is a CLI profile (docs/总体设计 第 6 节). Env values are secrets:
// they are sealed at rest and only ever shown to administrators.
type Profile struct {
	ID          int64             `json:"id"`
	NodeID      int64             `json:"node_id"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Mode        string            `json:"mode"`
	ShellPath   string            `json:"shell_path"`
	ShellArgs   []string          `json:"shell_args"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	ResumeCmd   string            `json:"resume_cmd"`
	ContinueCmd string            `json:"continue_cmd"`
	MyFolders   []string          `json:"my_folders,omitempty"` // the caller's folder range on this profile (docs/M10 第 3.4 节)
	Env         map[string]string `json:"env,omitempty"`
	DefaultCwd  string            `json:"default_cwd"`
	IdleTimeout int               `json:"idle_timeout"`
	QuoteStyle  string            `json:"quote_style"`
	Status      string            `json:"status"`
}

// Registry is the database side of this module.
type Registry struct {
	db   *store.DB
	seal *auth.Sealer
	now  func() time.Time
}

func NewRegistry(db *store.DB, seal *auth.Sealer, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{db: db, seal: seal, now: now}
}

func tokenHash(t string) []byte { h := sha256.Sum256([]byte(t)); return h[:] }

func adminOnly(id *auth.Identity) error {
	if !id.User.IsAdmin() {
		return ErrForbidden
	}
	if !id.Reverified {
		return auth.ErrReverifyRequired
	}
	return nil
}

// ---- nodes and tokens (docs/M7 第 3 节) ----

// CreateNode registers a node and returns its token, shown exactly once.
func (r *Registry) CreateNode(actor *auth.Identity, name, note string) (Node, string, error) {
	if err := adminOnly(actor); err != nil {
		return Node{}, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return Node{}, "", ErrBadRequest
	}
	res, err := r.db.Exec(`INSERT INTO nodes(name, note, created_at) VALUES (?,?,?)`, name, note, r.now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Node{}, "", ErrConflict
		}
		return Node{}, "", err
	}
	id, _ := res.LastInsertId()
	token, err := r.issueToken(id, false)
	return Node{ID: id, Name: name, Note: note, Status: "enabled"}, token, err
}

func (r *Registry) issueToken(nodeID int64, rotate bool) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := "thn_" + base64.RawURLEncoding.EncodeToString(b)
	if rotate { // old tokens live on for 24h, or until the new one is first used
		r.db.Exec(`UPDATE node_tokens SET expires_at=? WHERE node_id=? AND expires_at IS NULL`, r.now().Add(24*time.Hour).Unix(), nodeID)
	}
	_, err := r.db.Exec(`INSERT INTO node_tokens(token_hash,node_id,created_at) VALUES (?,?,?)`, tokenHash(token), nodeID, r.now().Unix())
	return token, err
}

// RotateToken issues a new token for a node.
func (r *Registry) RotateToken(actor *auth.Identity, nodeID int64) (string, error) {
	if err := adminOnly(actor); err != nil {
		return "", err
	}
	if _, err := r.node(nodeID); err != nil {
		return "", err
	}
	return r.issueToken(nodeID, true)
}

// AuthenticateNode resolves a node token before the WebSocket upgrade.
func (r *Registry) AuthenticateNode(token string) (Node, error) {
	if !strings.HasPrefix(token, "thn_") {
		return Node{}, auth.ErrUnauthenticated
	}
	var nodeID int64
	var seq int64 // rowid: insertion order, which a timestamp with one-second resolution cannot give
	h := tokenHash(token)
	err := r.db.QueryRow(`SELECT node_id, rowid FROM node_tokens WHERE token_hash=? AND (expires_at IS NULL OR expires_at>?)`,
		h, r.now().Unix()).Scan(&nodeID, &seq)
	if err != nil {
		return Node{}, auth.ErrUnauthenticated
	}
	n, err := r.node(nodeID)
	if err != nil || n.Status != "enabled" {
		return Node{}, auth.ErrUnauthenticated
	}
	// First use of a newer token retires every older one at once.
	r.db.Exec(`DELETE FROM node_tokens WHERE node_id=? AND rowid<?`, nodeID, seq)
	r.db.Exec(`UPDATE node_tokens SET last_used_at=? WHERE token_hash=?`, r.now().Unix(), h)
	return n, nil
}

const nodeCols = `id, name, note, status, fingerprint_mismatch, COALESCE(last_seen_at,0), agent_ver, host_ver, os_build, pwsh_store, position`

func scanNode(row interface{ Scan(...any) error }) (Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.Name, &n.Note, &n.Status, &n.FingerprintMismatch, &n.LastSeenAt, &n.AgentVer, &n.HostVer, &n.OSBuild, &n.PwshStore, &n.Position)
	return n, err
}

func (r *Registry) node(id int64) (Node, error) {
	n, err := scanNode(r.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

// CheckFingerprint records the fingerprint on first contact and rejects a
// different one afterwards (docs/M7 第 3 节).
func (r *Registry) CheckFingerprint(nodeID int64, fp string) bool {
	var known string
	if r.db.QueryRow(`SELECT fingerprint FROM nodes WHERE id=?`, nodeID).Scan(&known) != nil {
		return false
	}
	if known == "" {
		r.db.Exec(`UPDATE nodes SET fingerprint=?, fingerprint_mismatch=0 WHERE id=?`, fp, nodeID)
		return true
	}
	if known != fp {
		r.db.Exec(`UPDATE nodes SET fingerprint_mismatch=1 WHERE id=?`, nodeID)
		return false
	}
	return true
}

// ClearFingerprint lets the next connection register a new one (OS reinstalled).
func (r *Registry) ClearFingerprint(actor *auth.Identity, nodeID int64) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	_, err := r.db.Exec(`UPDATE nodes SET fingerprint='', fingerprint_mismatch=0 WHERE id=?`, nodeID)
	return err
}

func (r *Registry) noteHello(nodeID int64, m *proto.Msg) {
	r.db.Exec(`UPDATE nodes SET last_seen_at=?, agent_ver=?, host_ver=?, os_build=?, pwsh_store=? WHERE id=?`,
		r.now().Unix(), m.AgentVer, m.HostVer, m.OSBuild, m.PwshStore, nodeID)
}

// RenameNode changes a node's display name and note (the machine itself is
// identified by its token, not by the name).
// MoveNode sets a node's place in the lists (smaller first).
func (r *Registry) MoveNode(actor *auth.Identity, nodeID int64, position int) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	if position < 0 || position > 1000 {
		return ErrBadRequest
	}
	res, err := r.db.Exec(`UPDATE nodes SET position=? WHERE id=?`, position, nodeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Registry) RenameNode(actor *auth.Identity, nodeID int64, name, note string) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 || len(note) > 500 {
		return ErrBadRequest
	}
	res, err := r.db.Exec(`UPDATE nodes SET name=?, note=? WHERE id=?`, name, note, nodeID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrConflict
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNodeStatus enables or disables a node.
func (r *Registry) SetNodeStatus(actor *auth.Identity, nodeID int64, status string) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	if status != "enabled" && status != "disabled" {
		return ErrBadRequest
	}
	_, err := r.db.Exec(`UPDATE nodes SET status=? WHERE id=?`, status, nodeID)
	return err
}

// DeleteNode refuses while the node still has running sessions, so no session
// is left behind on a machine that can never connect again.
func (r *Registry) DeleteNode(actor *auth.Identity, nodeID int64) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	var running int
	r.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE node_id=? AND ended_at IS NULL`, nodeID).Scan(&running)
	if running > 0 {
		return ErrHasSessions
	}
	_, err := r.db.Exec(`DELETE FROM nodes WHERE id=?`, nodeID)
	return err
}

// VisibleNodes lists what the user may see: everything for an administrator,
// otherwise nodes that carry a profile bound to the user.
func (r *Registry) VisibleNodes(id *auth.Identity) ([]Node, error) {
	q := `SELECT ` + nodeCols + ` FROM nodes ORDER BY position, name`
	args := []any{}
	if !id.User.IsAdmin() {
		q = `SELECT ` + nodeCols + ` FROM nodes WHERE id IN (SELECT p.node_id FROM cli_profiles p
			JOIN profile_bindings b ON b.profile_id=p.id WHERE b.user_id=? AND p.status='enabled') ORDER BY position, name`
		args = append(args, id.User.ID)
	}
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CanUseNode is the permission behind the file functions (docs/M7 第 5 节).
func (r *Registry) CanUseNode(id *auth.Identity, nodeID int64) bool {
	if id.User.IsAdmin() {
		return true
	}
	var n int
	r.db.QueryRow(`SELECT COUNT(*) FROM cli_profiles p JOIN profile_bindings b ON b.profile_id=p.id
		WHERE p.node_id=? AND b.user_id=? AND p.status='enabled'`, nodeID, id.User.ID).Scan(&n)
	return n > 0
}

// ---- CLI profiles and bindings (docs/M7 第 4 节) ----

func (r *Registry) sealEnv(profileID int64, env map[string]string) []byte {
	if len(env) == 0 {
		return nil
	}
	b, _ := json.Marshal(env)
	return r.seal.Seal(b, fmt.Sprintf("cli_profiles.env:%d", profileID))
}

func (r *Registry) openEnv(profileID int64, sealed []byte) map[string]string {
	if len(sealed) == 0 {
		return nil
	}
	b, err := r.seal.Open(sealed, fmt.Sprintf("cli_profiles.env:%d", profileID))
	if err != nil {
		return nil
	}
	var env map[string]string
	json.Unmarshal(b, &env)
	return env
}

func validProfile(p *Profile) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 64 || (p.Mode != "shell" && p.Mode != "direct") {
		return ErrBadRequest
	}
	if p.Mode == "direct" && p.Command == "" || p.Mode == "shell" && p.ShellPath == "" {
		return ErrBadRequest
	}
	if p.IdleTimeout < 0 {
		return ErrBadRequest
	}
	if p.QuoteStyle == "" {
		p.QuoteStyle = "auto"
	}
	if p.Kind == "" {
		p.Kind = "custom"
	}
	return nil
}

// SaveProfile creates (ID 0) or updates a profile. Running sessions keep the
// snapshot they were created with.
func (r *Registry) SaveProfile(actor *auth.Identity, p Profile) (Profile, error) {
	if err := adminOnly(actor); err != nil {
		return p, err
	}
	if err := validProfile(&p); err != nil {
		return p, err
	}
	if _, err := r.node(p.NodeID); err != nil {
		return p, err
	}
	sa, _ := json.Marshal(append([]string{}, p.ShellArgs...))
	ar, _ := json.Marshal(append([]string{}, p.Args...))
	if p.ID == 0 {
		res, err := r.db.Exec(`INSERT INTO cli_profiles(node_id,name,kind,mode,shell_path,shell_args,command,args,resume_cmd,continue_cmd,default_cwd,idle_timeout,quote_style,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.NodeID, p.Name, p.Kind, p.Mode, p.ShellPath, string(sa), p.Command, string(ar),
			p.ResumeCmd, p.ContinueCmd, p.DefaultCwd, p.IdleTimeout, p.QuoteStyle, r.now().Unix())
		if err != nil {
			return p, err
		}
		p.ID, _ = res.LastInsertId()
		p.Status = "enabled"
	} else {
		res, err := r.db.Exec(`UPDATE cli_profiles SET name=?,kind=?,mode=?,shell_path=?,shell_args=?,command=?,args=?,resume_cmd=?,continue_cmd=?,default_cwd=?,idle_timeout=?,quote_style=? WHERE id=? AND node_id=?`,
			p.Name, p.Kind, p.Mode, p.ShellPath, string(sa), p.Command, string(ar), p.ResumeCmd, p.ContinueCmd, p.DefaultCwd, p.IdleTimeout, p.QuoteStyle, p.ID, p.NodeID)
		if err != nil {
			return p, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return p, ErrNotFound
		}
	}
	_, err := r.db.Exec(`UPDATE cli_profiles SET env=? WHERE id=?`, r.sealEnv(p.ID, p.Env), p.ID)
	return p, err
}

func (r *Registry) DeleteProfile(actor *auth.Identity, profileID int64) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	if _, err := r.db.Exec(`DELETE FROM cli_profiles WHERE id=?`, profileID); err != nil {
		return err
	}
	if _, err := r.db.Exec(`DELETE FROM history_hidden WHERE profile_id=?`, profileID); err != nil {
		return err
	}
	_, err := r.db.Exec(`DELETE FROM history_folders WHERE profile_id=?`, profileID)
	return err
}

// profileOrder lists profiles in their node's display order.
const profileOrder = `(SELECT n.position FROM nodes n WHERE n.id=cli_profiles.node_id), node_id, name`

const profileCols = `id,node_id,name,kind,mode,shell_path,shell_args,command,args,resume_cmd,continue_cmd,env,default_cwd,idle_timeout,quote_style,status`

func (r *Registry) scanProfile(row interface{ Scan(...any) error }, withEnv bool) (Profile, error) {
	var p Profile
	var sa, ar string
	var env []byte
	err := row.Scan(&p.ID, &p.NodeID, &p.Name, &p.Kind, &p.Mode, &p.ShellPath, &sa, &p.Command, &ar, &p.ResumeCmd, &p.ContinueCmd, &env, &p.DefaultCwd, &p.IdleTimeout, &p.QuoteStyle, &p.Status)
	json.Unmarshal([]byte(sa), &p.ShellArgs)
	json.Unmarshal([]byte(ar), &p.Args)
	if withEnv {
		p.Env = r.openEnv(p.ID, env)
	}
	return p, err
}

// Profiles lists the profiles a user may use. Environment values are included
// only for administrators.
func (r *Registry) Profiles(id *auth.Identity) ([]Profile, error) {
	q := `SELECT ` + profileCols + ` FROM cli_profiles ORDER BY ` + profileOrder
	args := []any{}
	if !id.User.IsAdmin() {
		q = `SELECT ` + profileCols + ` FROM cli_profiles WHERE status='enabled' AND id IN
			(SELECT profile_id FROM profile_bindings WHERE user_id=?) ORDER BY ` + profileOrder
		args = append(args, id.User.ID)
	}
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		p, err := r.scanProfile(rows, id.User.IsAdmin())
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].MyFolders = r.Folders(out[i].ID, id.User.ID)
	}
	return out, nil
}

// profileFor returns the full profile if the user may create sessions with it.
func (r *Registry) profileFor(id *auth.Identity, profileID int64) (Profile, error) {
	p, err := r.scanProfile(r.db.QueryRow(`SELECT `+profileCols+` FROM cli_profiles WHERE id=?`, profileID), true)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if id.User.IsAdmin() {
		return p, nil
	}
	var n int
	r.db.QueryRow(`SELECT COUNT(*) FROM profile_bindings WHERE profile_id=? AND user_id=?`, profileID, id.User.ID).Scan(&n)
	if n == 0 || p.Status != "enabled" {
		return p, ErrForbidden
	}
	return p, nil
}

// SetBindings replaces the set of users bound to a profile.
func (r *Registry) SetBindings(actor *auth.Identity, profileID int64, userIDs []int64, folders map[int64][]string) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	clean := map[int64]string{}
	if folders == nil {
		// A request without ranges (an older page, a script) keeps the ones
		// already set; only an explicit empty list clears a range (复核).
		keep, err := r.BindingFolders(profileID)
		if err != nil {
			return err
		}
		folders = keep
	}
	for uid, list := range folders {
		ok, err := cleanFolders(list)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(ok)
		clean[uid] = string(b)
	}
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM profile_bindings WHERE profile_id=?`, profileID); err != nil {
		return err
	}
	for _, uid := range userIDs {
		f := clean[uid]
		if f == "" {
			f = "[]"
		}
		if _, err := tx.Exec(`INSERT INTO profile_bindings(profile_id,user_id,folders) VALUES (?,?,?)`, profileID, uid, f); err != nil {
			return ErrBadRequest
		}
	}
	return tx.Commit()
}

// cleanFolders checks a folder range: absolute Windows folders, no trailing
// separator, no duplicates, a sane number and length.
func cleanFolders(list []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, f := range list {
		f = strings.TrimRight(strings.TrimSpace(f), `\/`)
		if f == "" {
			continue
		}
		if len(f) < 2 || len(f) > 1024 || f[1] != ':' || !(f[0] >= 'A' && f[0] <= 'Z' || f[0] >= 'a' && f[0] <= 'z') ||
			(len(f) > 2 && f[2] != '\\' && f[2] != '/') || strings.ContainsAny(f, "\x00*?\"<>|") {
			return nil, &auth.Error{Code: "bad_folder", Msg: "文件夹范围要写完整路径，如 C:\\Users\\me\\项目：" + f, Status: 400}
		}
		f = strings.ReplaceAll(f, "/", `\`)
		if k := strings.ToLower(f); !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	if len(out) > 20 {
		return nil, &auth.Error{Code: "bad_folder", Msg: "文件夹范围最多 20 个", Status: 400}
	}
	return out, nil
}

// Folders is a user's folder range on a profile; nil: no binding or no range
// (all). When the range cannot be read, the answer is a range nothing lies
// in: a failure shows less, never more (复核).
func (r *Registry) Folders(profileID, userID int64) []string {
	var s string
	err := r.db.QueryRow(`SELECT folders FROM profile_bindings WHERE profile_id=? AND user_id=?`, profileID, userID).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	var out []string
	if err != nil || json.Unmarshal([]byte(s), &out) != nil {
		return []string{noFolder}
	}
	return out
}

// historyRange is what of a profile's history a user may see, asked again
// when the node's answer is in: a binding removed meanwhile refuses it
// instead of lifting the range (问题单 1 第 2 条). nil: all (an admin with no
// binding, or a binding without a range).
func (r *Registry) historyRange(id *auth.Identity, profileID int64) ([]string, error) {
	if _, err := r.profileFor(id, profileID); err != nil {
		return nil, err
	}
	var s string
	err := r.db.QueryRow(`SELECT folders FROM profile_bindings WHERE profile_id=? AND user_id=?`, profileID, id.User.ID).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		if id.User.IsAdmin() {
			return nil, nil
		}
		return nil, ErrForbidden
	}
	var out []string
	if err != nil || json.Unmarshal([]byte(s), &out) != nil {
		return []string{noFolder}, nil // a failure shows less, never more
	}
	return out, nil
}

// noFolder is a range entry that no real folder is in.
const noFolder = `?:\\nothing`

// BindingFolders lists the folder ranges of a profile's bindings, by user.
func (r *Registry) BindingFolders(profileID int64) (map[int64][]string, error) {
	rows, err := r.db.Query(`SELECT user_id, folders FROM profile_bindings WHERE profile_id=?`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var uid int64
		var s string
		rows.Scan(&uid, &s)
		var list []string
		json.Unmarshal([]byte(s), &list)
		if len(list) > 0 {
			out[uid] = list
		}
	}
	return out, rows.Err()
}

// inFolders: a folder is the range's folder or below it (Windows: any case).
func inFolders(cwd string, folders []string) bool {
	if len(folders) == 0 {
		return true
	}
	c := normFolder(cwd)
	if c == "" || strings.Contains(c+`\`, `\..\`) {
		return false // unknown, or not a plain absolute folder: not shown
	}
	for _, f := range folders {
		f = normFolder(f)
		if c == f || strings.HasPrefix(c, f+`\`) {
			return true
		}
	}
	return false
}

// normFolder: lower case, backslashes, no \\?\ prefix, no doubled or trailing separators.
func normFolder(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, `\`, "/"))
	s = strings.TrimPrefix(s, "//?/")
	// . and .. resolved as well: C:\work\sub\.. is C:\work (one CLI per folder, folder ranges)
	s = path.Clean("/" + s)[1:]
	return strings.TrimRight(strings.ReplaceAll(s, "/", `\`), `\`)
}

func (r *Registry) Bindings(profileID int64) ([]int64, error) {
	rows, err := r.db.Query(`SELECT user_id FROM profile_bindings WHERE profile_id=?`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- session register ----

// SessionRow is the Hub's record of a session; the truth lives on the node.
type SessionRow struct {
	Kind        string `json:"kind"` // the CLI kind when this session started
	SID         string `json:"sid"`
	NodeID      int64  `json:"node_id"`
	ProfileID   int64  `json:"profile_id"`
	ProfileName string `json:"profile_name"`
	OwnerID     int64  `json:"owner_id"`
	Cwd         string `json:"cwd"`
	Title       string `json:"title"`
	CreatedAt   int64  `json:"created_at"`
	EndedAt     int64  `json:"ended_at"`
	EndReason   string `json:"end_reason"`
	ExitCode    *int   `json:"exit_code"`
	ConvID      string `json:"conv_id,omitempty"` // the CLI conversation it reopened, when known (docs/M10 第 3 节)
}

const sessionCols = `sid,node_id,COALESCE(profile_id,0),profile_name,COALESCE(owner_id,0),cwd,title,created_at,COALESCE(ended_at,0),end_reason,exit_code,conv_id,kind`

func scanSession(row interface{ Scan(...any) error }) (SessionRow, error) {
	var s SessionRow
	err := row.Scan(&s.SID, &s.NodeID, &s.ProfileID, &s.ProfileName, &s.OwnerID, &s.Cwd, &s.Title, &s.CreatedAt, &s.EndedAt, &s.EndReason, &s.ExitCode, &s.ConvID, &s.Kind)
	return s, err
}

func (r *Registry) session(sid string) (SessionRow, error) {
	s, err := scanSession(r.db.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE sid=?`, sid))
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// CanAccessSession: an administrator, or the creator while still bound to
// the session's CLI profile — losing the binding loses the shell, not only
// the open connection (docs/M7 第 5 节, 复核第四轮 4).
func (r *Registry) CanAccessSession(id *auth.Identity, s SessionRow) bool {
	if id.User.IsAdmin() {
		return true
	}
	if s.OwnerID != id.User.ID {
		return false
	}
	if s.ProfileID == 0 {
		return true
	}
	var n int
	r.db.QueryRow(`SELECT COUNT(*) FROM profile_bindings b JOIN cli_profiles p ON p.id=b.profile_id
		WHERE b.profile_id=? AND b.user_id=? AND p.status='enabled'`, s.ProfileID, id.User.ID).Scan(&n)
	return n > 0
}

// RunningSessions lists a user's running sessions, or everyone's for an
// administrator asking for all.
func (r *Registry) RunningSessions(id *auth.Identity, all bool) ([]SessionRow, error) {
	q, args := `SELECT `+sessionCols+` FROM sessions WHERE ended_at IS NULL AND owner_id=? ORDER BY created_at DESC`, []any{id.User.ID}
	if all && id.User.IsAdmin() {
		q, args = `SELECT `+sessionCols+` FROM sessions WHERE ended_at IS NULL ORDER BY created_at DESC`, nil
	}
	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Registry) countRunning(where string, arg any) (n int) {
	r.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE ended_at IS NULL AND `+where+`=?`, arg).Scan(&n)
	return n
}
