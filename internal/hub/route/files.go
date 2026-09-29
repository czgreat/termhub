package route

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"termhub/internal/hub/auth"
	"termhub/internal/proto"
)

// fileStatus gives the node's file error codes (docs/M4 第 9 节) an HTTP status.
var fileStatus = map[string]int{"not_found": 404, "access_denied": 403, "exists": 409, "path_invalid": 400,
	"name_invalid": 400, "not_a_directory": 400, "unc_disabled": 403, "unsupported": 400, "bad_request": 400}

// nodeFile sends one browsing request to a node on behalf of a user who may
// use that node (docs/M7 第 5 节), and hands the node's answer back unchanged.
func (h *Hub) nodeFile(id *auth.Identity, nodeID int64, typ string, params map[string]any) (json.RawMessage, error) {
	if _, err := h.reg.node(nodeID); err != nil || !h.reg.CanUseNode(id, nodeID) {
		return nil, ErrNotFound // a node you may not use looks like a node that does not exist
	}
	n := h.nodeConn(nodeID)
	if n == nil {
		return nil, ErrNodeOffline
	}
	payload, _ := json.Marshal(params)
	r, err := n.request(&proto.Msg{T: typ, Payload: payload}, 20*time.Second)
	if err != nil {
		var e *auth.Error
		if errors.As(err, &e) {
			if st, ok := fileStatus[e.Code]; ok {
				return nil, &auth.Error{Code: e.Code, Msg: e.Msg, Status: st}
			}
		}
		return nil, err
	}
	return r.Payload, nil
}

func (h *Hub) registerFiles(mux *http.ServeMux) {
	handle := func(pattern, typ string, params func(w http.ResponseWriter, r *http.Request) (map[string]any, error), audit string) {
		mux.Handle(pattern, h.web.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := auth.IdentityFrom(r.Context())
			nodeID, err := pathID(r, "id")
			var p map[string]any
			if err == nil {
				p, err = params(w, r)
			}
			var out json.RawMessage
			if err == nil {
				out, err = h.nodeFile(id, nodeID, typ, p)
			}
			if audit != "" { // every change made through the file functions is recorded, failed ones too
				result := "ok"
				if err != nil {
					result = errCode(err)
				}
				h.web.S.Audit(&id.User, id.IP, audit, "node "+strconv.FormatInt(nodeID, 10)+": "+pathOf(p), result, "")
			}
			if err != nil {
				h.web.Fail(w, r, err)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(out)
		})))
	}
	handle("GET /api/nodes/{id}/fs/drives", proto.MsgFsList, func(http.ResponseWriter, *http.Request) (map[string]any, error) {
		return map[string]any{"path": ""}, nil
	}, "")
	handle("GET /api/nodes/{id}/fs/list", proto.MsgFsList, func(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
		q := r.URL.Query()
		if q.Get("path") == "" {
			return nil, ErrBadRequest
		}
		page := 0
		if raw := q.Get("page"); raw != "" {
			var err error
			page, err = strconv.Atoi(raw)
			if err != nil || page < 0 || page > 1_000_000 {
				return nil, ErrBadRequest
			}
		}
		return map[string]any{"path": q.Get("path"), "page": page, "hidden": q.Get("hidden") == "1"}, nil
	}, "")
	handle("GET /api/nodes/{id}/fs/stat", proto.MsgFsStat, func(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
		if r.URL.Query().Get("path") == "" {
			return nil, ErrBadRequest
		}
		return map[string]any{"path": r.URL.Query().Get("path")}, nil
	}, "")
	handle("POST /api/nodes/{id}/fs/mkdir", proto.MsgFsMkdir, func(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
		var in struct{ Parent, Name string }
		if err := readBody(w, r, &in); err != nil {
			return nil, err
		}
		return map[string]any{"path": in.Parent, "name": in.Name}, nil
	}, "fs_mkdir")
}

func pathOf(p map[string]any) string {
	s, _ := p["path"].(string)
	if n, _ := p["name"].(string); n != "" {
		s += `\` + n
	}
	return s
}
