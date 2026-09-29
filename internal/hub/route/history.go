package route

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"termhub/internal/hub/auth"
	"termhub/internal/proto"
)

// History (docs/M10 第 3.2 节): the node reads a CLI's own conversation files,
// read-only, and the Hub passes the answer through. The Hub keeps nothing of
// it but the list of conversations each user chose to hide; titles and
// messages are neither stored nor logged.

// historyEnv is the part of a profile's environment that says where the
// CLI keeps its files; nothing else of the (secret) environment is sent.
var historyEnv = []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"}

func historyParams(p Profile) (map[string]any, error) {
	if p.Kind != "claude" && p.Kind != "codex" {
		return nil, &auth.Error{Code: "unsupported", Msg: "这个 CLI 配置没有可读的历史记录", Status: 400}
	}
	env := map[string]string{}
	for k, v := range p.Env { // any case of the name: the session's environment ignores it too
		for _, want := range historyEnv {
			if strings.EqualFold(k, want) {
				env[want] = v
			}
		}
	}
	return map[string]any{"kind": p.Kind, "env": env}, nil
}

type historyList struct {
	Items      []map[string]any `json:"items"`
	Scanned    int              `json:"scanned"`
	Failed     int              `json:"failed"`
	Incomplete bool             `json:"incomplete"`
	Fallback   bool             `json:"fallback"`
	Reason     string           `json:"reason,omitempty"`
	Source     string           `json:"source,omitempty"` // which folder the node read, as a digest
	Hidden     int              `json:"hidden"`           // how many this user hid
	Running    []runningConv    `json:"running"`          // this user's live sessions of this profile
	// this user's folder settings: hidden folders (their conversations are
	// left out of items) and display names, both keyed by the normalised folder
	HiddenFolders []folderPref      `json:"hidden_folders"`
	FolderNames   map[string]string `json:"folder_names"`
	// the newest conversation time of every folder in range, hidden ones
	// included: "continue" in a hidden folder still resumes its newest
	// conversation, and the page must be able to warn that it may be going on
	// elsewhere (复核)
	Latest map[string]latestConv `json:"latest"`
}

// maxFolderPrefs bounds one user's folder settings on one profile (复核).
const maxFolderPrefs = 500

type latestConv struct {
	ID      string `json:"id"`
	Updated int64  `json:"updated"`
}

type folderPref struct {
	Folder string `json:"folder"` // as the conversations give it
	Key    string `json:"key"`
	Name   string `json:"name,omitempty"`
	Count  int    `json:"count"`
}

// folderPrefs reads a user's folder settings on a profile, by normalised folder.
func (h *Hub) folderPrefs(userID, profileID int64) (hidden map[string]bool, names map[string]string) {
	hidden, names = map[string]bool{}, map[string]string{}
	rows, err := h.reg.db.Query(`SELECT folder, hidden, name FROM history_folders WHERE user_id=? AND profile_id=?`, userID, profileID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var f, n string
		var hid bool
		rows.Scan(&f, &hid, &n)
		if hid {
			hidden[f] = true
		}
		if n != "" {
			names[f] = n
		}
	}
	return
}

type runningConv struct {
	SID  string `json:"sid"`
	Cwd  string `json:"cwd"`
	Conv string `json:"conv_id,omitempty"`
}

func (h *Hub) hiddenConvs(userID, profileID int64) map[string]bool {
	out := map[string]bool{}
	rows, err := h.reg.db.Query(`SELECT conv_id FROM history_hidden WHERE user_id=? AND profile_id=?`, userID, profileID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		rows.Scan(&c)
		out[c] = true
	}
	return out
}

func (h *Hub) registerHistory(mux *http.ServeMux) {
	handle := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, id *auth.Identity, p Profile) error) {
		mux.Handle(pattern, h.web.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := auth.IdentityFrom(r.Context())
			err := func() error {
				pid, err := pathID(r, "id")
				if err != nil {
					return err
				}
				p, err := h.reg.profileFor(id, pid)
				if err != nil {
					return err
				}
				if c := r.PathValue("conv"); c != "" && !sessionIDPattern.MatchString(c) {
					return ErrBadRequest
				}
				return fn(w, r, id, p)
			}()
			if err != nil {
				h.web.Fail(w, r, err)
			}
		})))
	}

	// The conversations of a profile, newest first; ?hidden=1 lists the hidden ones instead.
	handle("GET /api/profiles/{id}/history", func(w http.ResponseWriter, r *http.Request, id *auth.Identity, p Profile) error {
		params, err := historyParams(p)
		if err != nil {
			return err
		}
		raw, err := h.nodeFile(id, p.NodeID, proto.MsgHistoryList, params)
		if err != nil {
			return err
		}
		var list historyList
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		folders, err := h.reg.historyRange(id, p.ID)
		if err != nil {
			return err
		}
		hidden, wantHidden := h.hiddenConvs(id.User.ID, p.ID), r.URL.Query().Get("hidden") == "1"
		hiddenDirs, names := h.folderPrefs(id.User.ID, p.ID)
		list.FolderNames = names
		list.Latest = map[string]latestConv{}
		list.HiddenFolders = []folderPref{}
		seenDir := map[string]int{}
		items := make([]map[string]any, 0, len(list.Items))
		for _, it := range list.Items {
			conv, _ := it["id"].(string)
			cwd, _ := it["cwd"].(string)
			if !inFolders(cwd, folders) {
				continue // outside this user's range: not even its title leaves the Hub
			}
			if k := normFolder(cwd); k != "" {
				if u, _ := it["updated"].(float64); int64(u) > list.Latest[k].Updated {
					list.Latest[k] = latestConv{ID: conv, Updated: int64(u)}
				}
			}
			if k := normFolder(cwd); hiddenDirs[k] {
				if i, ok := seenDir[k]; ok {
					list.HiddenFolders[i].Count++
				} else {
					seenDir[k] = len(list.HiddenFolders)
					list.HiddenFolders = append(list.HiddenFolders, folderPref{Folder: cwd, Key: k, Name: names[k], Count: 1})
				}
				continue // the whole folder is hidden from this user's list
			}
			if hidden[conv] {
				list.Hidden++
			}
			if hidden[conv] == wantHidden {
				items = append(items, it)
			}
		}
		list.Items = items
		list.Running = []runningConv{}
		rows, err := h.reg.db.Query(`SELECT sid, cwd, conv_id FROM sessions WHERE ended_at IS NULL AND owner_id=? AND profile_id=?`, id.User.ID, p.ID)
		if err == nil {
			for rows.Next() {
				var c runningConv
				rows.Scan(&c.SID, &c.Cwd, &c.Conv)
				list.Running = append(list.Running, c)
			}
			rows.Close()
		}
		writeJSON(w, list)
		return nil
	})

	// One conversation's messages, the latest first page; ?before=<cursor> pages back.
	handle("GET /api/profiles/{id}/history/{conv}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity, p Profile) error {
		params, err := historyParams(p)
		if err != nil {
			return err
		}
		params["id"] = r.PathValue("conv")
		if b := r.URL.Query().Get("before"); b != "" {
			n, err := strconv.ParseInt(b, 10, 64)
			if err != nil || n < 0 {
				return ErrBadRequest
			}
			params["before"] = n
		}
		raw, err := h.nodeFile(id, p.NodeID, proto.MsgHistoryRead, params)
		if err == nil {
			var t struct{ Cwd string }
			var folders []string
			if folders, err = h.reg.historyRange(id, p.ID); err != nil {
				raw = nil
			} else if json.Unmarshal(raw, &t) != nil || !inFolders(t.Cwd, folders) {
				raw, err = nil, ErrNotFound // outside this user's range
			}
		}
		result := "ok"
		if err != nil {
			result = errCode(err)
		}
		if r.URL.Query().Get("before") == "" { // opening it, not every page of it
			h.web.S.Audit(&id.User, id.IP, "history_read", p.Name+": "+r.PathValue("conv"), result, "")
		}
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(raw)
		return nil
	})

	// A folder hidden or renamed in this user's list; nothing on the node changes.
	handle("PUT /api/profiles/{id}/history-folder", func(w http.ResponseWriter, r *http.Request, id *auth.Identity, p Profile) error {
		var in struct {
			Folder string  `json:"folder"`
			Hidden *bool   `json:"hidden"`
			Name   *string `json:"name"`
		}
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		// check everything first, then write once: a refused request leaves nothing behind
		k := normFolder(in.Folder)
		if k == "" || len(k) > 1024 || (in.Hidden == nil && in.Name == nil) {
			return ErrBadRequest
		}
		var name any // NULL: leave as it is
		if in.Name != nil {
			n := strings.Map(func(r rune) rune {
				if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
					return -1
				}
				return r
			}, strings.TrimSpace(*in.Name))
			if utf8.RuneCountInString(n) > 60 {
				return &auth.Error{Code: "bad_name", Msg: "名字最多 60 个字", Status: 400}
			}
			name = n
		}
		var hidden any
		if in.Hidden != nil {
			hidden = *in.Hidden
		}
		tx, err := h.reg.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var exists, count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM history_folders WHERE user_id=? AND profile_id=? AND folder=?`, id.User.ID, p.ID, k).Scan(&exists); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM history_folders WHERE user_id=? AND profile_id=?`, id.User.ID, p.ID).Scan(&count); err != nil {
			return err
		}
		clears := (in.Hidden == nil || !*in.Hidden) && (in.Name == nil || name == "")
		if exists == 0 && clears { // nothing set, nothing to set
			writeJSON(w, map[string]bool{"ok": true})
			return nil
		}
		if exists == 0 && count >= maxFolderPrefs {
			return &auth.Error{Code: "too_many", Msg: "隐藏或改名的文件夹太多了（每个配置最多 500 个）", Status: 400}
		}
		if _, err := tx.Exec(`INSERT INTO history_folders(user_id, profile_id, folder, hidden, name) VALUES (?,?,?,COALESCE(?,0),COALESCE(?,''))
			ON CONFLICT(user_id, profile_id, folder) DO UPDATE SET hidden=COALESCE(?,hidden), name=COALESCE(?,name)`,
			id.User.ID, p.ID, k, hidden, name, hidden, name); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM history_folders WHERE user_id=? AND profile_id=? AND folder=? AND hidden=0 AND name=''`, id.User.ID, p.ID, k); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if in.Hidden != nil {
			action := "history_folder_unhide"
			if *in.Hidden {
				action = "history_folder_hide"
			}
			h.web.S.Audit(&id.User, id.IP, action, p.Name+": "+k, "ok", "")
		}
		if in.Name != nil {
			h.web.S.Audit(&id.User, id.IP, "history_folder_rename", p.Name+": "+k, "ok", "")
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})

	// Hiding is this user's view only: the CLI's files stay as they are.
	handle("PUT /api/profiles/{id}/history/{conv}/hidden", func(w http.ResponseWriter, r *http.Request, id *auth.Identity, p Profile) error {
		if _, err := h.reg.db.Exec(`INSERT OR IGNORE INTO history_hidden(user_id, profile_id, conv_id, hidden_at) VALUES (?,?,?,?)`,
			id.User.ID, p.ID, r.PathValue("conv"), time.Now().Unix()); err != nil {
			return err
		}
		h.web.S.Audit(&id.User, id.IP, "history_hide", p.Name+": "+r.PathValue("conv"), "ok", "")
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	handle("DELETE /api/profiles/{id}/history/{conv}/hidden", func(w http.ResponseWriter, r *http.Request, id *auth.Identity, p Profile) error {
		if _, err := h.reg.db.Exec(`DELETE FROM history_hidden WHERE user_id=? AND profile_id=? AND conv_id=?`,
			id.User.ID, p.ID, r.PathValue("conv")); err != nil {
			return err
		}
		h.web.S.Audit(&id.User, id.IP, "history_unhide", p.Name+": "+r.PathValue("conv"), "ok", "")
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
}
