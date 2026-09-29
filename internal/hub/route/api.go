package route

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"termhub/internal/hub/auth"
	"termhub/internal/proto"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func readBody(w http.ResponseWriter, r *http.Request, v any) error {
	// a dripping body holds nothing but its own connection (安全复核 L7)
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	b, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil || json.Unmarshal(b, v) != nil {
		return ErrBadRequest
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		return 0, ErrBadRequest
	}
	return id, nil
}

// Register mounts docs/M7 第 10 节. Every REST endpoint goes through
// auth.Require (session, forced steps, CSRF and Origin on writes); the
// WebSocket endpoints authenticate themselves before upgrading.
func (h *Hub) Register(mux *http.ServeMux) {
	handle := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error) {
		mux.Handle(pattern, h.web.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r, auth.IdentityFrom(r.Context())); err != nil {
				h.web.Fail(w, r, err)
			}
		})))
	}
	h.registerFiles(mux)
	h.registerTransfers(mux)
	h.registerProjects(mux)
	h.registerHistory(mux)
	h.registerUsage(handle)
	mux.HandleFunc("GET /node/link", h.ServeNode)
	mux.HandleFunc("GET /ws/events", h.ServeEvents)
	mux.HandleFunc("GET /ws/session/{sid}", h.ServeSession)

	handle("GET /api/nodes", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		nodes, err := h.reg.VisibleNodes(id)
		if err != nil {
			return err
		}
		for i := range nodes {
			nodes[i].Online = h.Online(nodes[i].ID)
		}
		writeJSON(w, map[string]any{"nodes": nodes})
		return nil
	})
	handle("GET /api/profiles", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		list, err := h.reg.Profiles(id)
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{"profiles": list})
		return nil
	})
	handle("GET /api/sessions", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		list, err := h.reg.RunningSessions(id, r.URL.Query().Get("all") == "1")
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{"sessions": list})
		return nil
	})
	handle("POST /api/sessions", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		var in NewSession
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		row, err := h.CreateSession(id, in)
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{"session": row})
		return nil
	})
	handle("POST /api/sessions/{sid}/take", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		if err := h.TakeSession(id, r.PathValue("sid")); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	handle("DELETE /api/sessions/{sid}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		if err := h.CloseSession(id, r.PathValue("sid"), r.URL.Query().Get("mode") == "force"); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})

	// administration
	handle("POST /api/admin/nodes", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		var in struct{ Name, Note string }
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		n, token, err := h.reg.CreateNode(id, in.Name, in.Note)
		if err != nil {
			return err
		}
		h.web.S.Audit(&id.User, id.IP, "node_created", n.Name, "ok", "")
		writeJSON(w, map[string]any{"node": n, "token": token}) // the only time the token is ever shown
		return nil
	})
	handle("POST /api/admin/nodes/{id}/{action}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		nid, err := pathID(r, "id")
		if err != nil {
			return err
		}
		out := map[string]any{"ok": true}
		action := r.PathValue("action")
		switch action {
		case "rotate-token":
			var t string
			t, err = h.reg.RotateToken(id, nid)
			out["token"] = t
		case "clear-fingerprint":
			err = h.reg.ClearFingerprint(id, nid)
		case "disable", "enable":
			status := map[string]string{"disable": "disabled", "enable": "enabled"}[action]
			if err = h.reg.SetNodeStatus(id, nid, status); err == nil && status == "disabled" {
				if n := h.nodeConn(nid); n != nil {
					n.ws.close()
				}
			}
		default:
			err = ErrNotFound
		}
		if err != nil {
			return err
		}
		h.web.S.Audit(&id.User, id.IP, "node_"+action, strconv.FormatInt(nid, 10), "ok", "")
		writeJSON(w, out)
		return nil
	})
	handle("PATCH /api/admin/nodes/{id}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		nid, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in struct {
			Name, Note string
			Position   *int
		}
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		if in.Name == "" && in.Position == nil {
			return ErrBadRequest
		}
		if in.Name != "" {
			if err := h.reg.RenameNode(id, nid, in.Name, in.Note); err != nil {
				return err
			}
			h.web.S.Audit(&id.User, id.IP, "node_renamed", strconv.FormatInt(nid, 10), "ok", in.Name)
		}
		if in.Position != nil {
			if err := h.reg.MoveNode(id, nid, *in.Position); err != nil {
				return err
			}
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	handle("DELETE /api/admin/nodes/{id}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		nid, err := pathID(r, "id")
		if err != nil {
			return err
		}
		if err := h.reg.DeleteNode(id, nid); err != nil {
			return err
		}
		if n := h.nodeConn(nid); n != nil {
			n.ws.close()
		}
		h.web.S.Audit(&id.User, id.IP, "node_deleted", strconv.FormatInt(nid, 10), "ok", "")
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	handle("POST /api/admin/profiles", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		var p Profile
		if err := readBody(w, r, &p); err != nil {
			return err
		}
		saved, err := h.reg.SaveProfile(id, p)
		if err != nil {
			return err
		}
		h.web.S.Audit(&id.User, id.IP, "profile_saved", saved.Name, "ok", "") // env values never reach the audit log
		writeJSON(w, map[string]any{"profile": saved})
		return nil
	})
	handle("DELETE /api/admin/profiles/{id}", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		pid, err := pathID(r, "id")
		if err != nil {
			return err
		}
		if err := h.reg.DeleteProfile(id, pid); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	handle("GET /api/admin/profiles/{id}/bindings", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		if !id.User.IsAdmin() {
			return ErrForbidden
		}
		pid, err := pathID(r, "id")
		if err != nil {
			return err
		}
		ids, err := h.reg.Bindings(pid)
		if err != nil {
			return err
		}
		folders, err := h.reg.BindingFolders(pid)
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{"user_ids": ids, "folders": folders})
		return nil
	})
	handle("PUT /api/admin/profiles/{id}/bindings", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		pid, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in struct {
			UserIDs []int64            `json:"user_ids"`
			Folders map[int64][]string `json:"folders"` // per user; missing or empty: all
		}
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		if err := h.reg.SetBindings(id, pid, in.UserIDs, in.Folders); err != nil {
			return err
		}
		h.kickUnbound(pid)
		h.web.S.Audit(&id.User, id.IP, "bindings_set", strconv.FormatInt(pid, 10), "ok", "")
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
}

// kickUnbound closes the terminal connections of users who just lost their
// binding to a profile. The sessions keep running; an administrator decides
// what happens to them (docs/M7 第 5 节).
func (h *Hub) kickUnbound(profileID int64) {
	bound := map[int64]bool{}
	ids, _ := h.reg.Bindings(profileID)
	for _, id := range ids {
		bound[id] = true
	}
	rows, err := h.reg.db.Query(`SELECT sid FROM sessions WHERE profile_id=? AND ended_at IS NULL`, profileID)
	if err != nil {
		return
	}
	var sids []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		sids = append(sids, s)
	}
	rows.Close()
	for _, s := range sids {
		sid, err := proto.ParseSID(s)
		if err != nil {
			continue
		}
		if rt := h.route(sid); rt != nil {
			rt.each(func(a *attachment) {
				if !a.id.User.IsAdmin() && !bound[a.id.User.ID] {
					a.ws.close()
				}
			})
		}
	}
}
