package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	cookieSession = "__Host-th_sess"
	cookieDevice  = "__Host-th_dev"
	headerCSRF    = "X-TH-CSRF"
)

// HTTP exposes the Service (docs/M6 第 5、6、12、13 节).
type HTTP struct {
	S *Service
	// Origins holds scheme://host[:port] of every public URL: the LAN address
	// and, later, the address behind the reverse proxy. Every state-changing
	// request and every WebSocket upgrade must carry exactly one of them.
	Origins map[string]bool
	// ClientIP extracts the caller's address, honouring trusted proxies (docs/M8).
	ClientIP func(*http.Request) string
	Log      *slog.Logger
}

// NewHTTP validates the public URL. It must be https: the __Host- cookies
// used here are only accepted by browsers over TLS.
func NewHTTP(s *Service, publicURL string, clientIP func(*http.Request) string, log *slog.Logger) (*HTTP, error) {
	origins := map[string]bool{}
	for _, one := range strings.Split(publicURL, ",") {
		u, err := url.Parse(strings.TrimSpace(one))
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, errors.New("auth: every public URL must be an https URL, got " + one)
		}
		origins[u.Scheme+"://"+u.Host] = true
	}
	if clientIP == nil {
		clientIP = func(r *http.Request) string {
			host, _, _ := strings.Cut(r.RemoteAddr, ":")
			return host
		}
	}
	if log == nil {
		log = slog.Default()
	}
	return &HTTP{S: s, Origins: origins, ClientIP: clientIP, Log: log}, nil
}

type ctxKey struct{}

// IdentityFrom returns the authenticated identity of a request that passed Require.
func IdentityFrom(ctx context.Context) *Identity {
	id, _ := ctx.Value(ctxKey{}).(*Identity)
	return id
}

// SecurityHeaders wraps the whole Hub (docs/M6 第 13 节). A terminal page
// framed by another site means arbitrary command execution, so framing is
// forbidden outright.
func SecurityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
		"font-src 'self'; connect-src 'self'; worker-src 'self'; manifest-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Fail writes an error in the Hub's uniform shape; other modules use it too.
func (h *HTTP) Fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		h.Log.Error("internal error", "path", r.URL.Path, "err", err)
		e = &Error{"internal", "服务器内部错误", 500}
	}
	writeJSON(w, e.Status, map[string]string{"code": e.Code, "msg": e.Msg})
}

// readJSON accepts only application/json. A cross-site form cannot send that
// content type without a CORS preflight, which this server never grants.
func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		return ErrBadRequest
	}
	// A body must arrive within a moment: a connection that drips bytes holds
	// nothing but itself (安全复核 L7).
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || json.Unmarshal(body, v) != nil {
		return ErrBadRequest
	}
	return nil
}

func (h *HTTP) sameOrigin(r *http.Request) bool { return h.Origins[r.Header.Get("Origin")] }

func (h *HTTP) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
	if value == "" {
		c.MaxAge = -1
	} else if maxAge > 0 {
		c.MaxAge = int(maxAge.Seconds())
	}
	http.SetCookie(w, c)
}

func cookieValue(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	return ""
}

// open wraps endpoints reachable without a session: they still demand the
// right Origin and a JSON body.
func (h *HTTP) open(fn func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.sameOrigin(r) {
			h.Fail(w, r, ErrForbidden)
			return
		}
		if err := fn(w, r); err != nil {
			h.Fail(w, r, err)
		}
	}
}

// firstLoginPaths stay reachable while forced first-login steps are open.
var firstLoginPaths = map[string]bool{"/api/me": true, "/api/auth/logout": true, "/api/me/password": true,
	"/api/me/totp/begin": true, "/api/me/totp/confirm": true}

// Require authenticates a request, enforces the forced first-login steps and,
// for anything that is not a read, the CSRF token and the Origin. Handlers of
// every Hub module are wrapped with it.
func (h *HTTP) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := h.S.Authenticate(cookieValue(r, cookieSession), h.ClientIP(r))
		if err != nil {
			h.Fail(w, r, err)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !h.sameOrigin(r) || !hmacEqual(r.Header.Get(headerCSRF), id.CSRF) {
				h.Fail(w, r, &Error{"csrf", "请求来源校验失败", 403})
				return
			}
		}
		if len(id.Pending) > 0 && !firstLoginPaths[r.URL.Path] {
			h.Fail(w, r, ErrStepsPending)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// AuthenticateWS is called before a WebSocket upgrade: cookie plus Origin.
// Browsers always send Origin on WebSocket handshakes, and they do not apply
// the same-origin policy to them, so this check is the only protection.
func (h *HTTP) AuthenticateWS(r *http.Request) (*Identity, error) {
	if !h.sameOrigin(r) {
		return nil, ErrForbidden
	}
	id, err := h.S.Authenticate(cookieValue(r, cookieSession), h.ClientIP(r))
	if err != nil {
		return nil, err
	}
	if len(id.Pending) > 0 {
		return nil, ErrStepsPending
	}
	return id, nil
}

func (h *HTTP) authed(fn func(w http.ResponseWriter, r *http.Request, id *Identity) error) http.Handler {
	return h.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r, IdentityFrom(r.Context())); err != nil {
			h.Fail(w, r, err)
		}
	}))
}

type userView struct {
	ID            int64  `json:"id"`
	Username      string `json:"username"`
	DisplayName   string `json:"display_name"`
	Role          string `json:"role"`
	Status        string `json:"status"`
	TOTPConfirmed bool   `json:"totp_confirmed"`
	MustChange    bool   `json:"must_change_password"`
	CreatedAt     int64  `json:"created_at"`
	LastLoginAt   int64  `json:"last_login_at"`
}

func viewUser(u User) userView {
	return userView{u.ID, u.Username, u.DisplayName, u.Role, u.Status, u.TOTPConfirmed, u.MustChangePassword, u.CreatedAt, u.LastLoginAt}
}

// Register mounts the endpoints of docs/M6 第 12 节.
func (h *HTTP) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"needed": h.S.SetupToken() != ""})
	})
	mux.HandleFunc("POST /api/setup", h.open(func(w http.ResponseWriter, r *http.Request) error {
		var in struct{ Token, Username, Password string }
		if err := readJSON(w, r, &in); err != nil {
			return err
		}
		if err := h.S.Setup(in.Token, in.Username, in.Password, h.ClientIP(r)); err != nil {
			return err
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/auth/login", h.open(func(w http.ResponseWriter, r *http.Request) error {
		var in struct{ Username, Password string }
		if err := readJSON(w, r, &in); err != nil {
			return err
		}
		res, err := h.S.Login(in.Username, in.Password, h.ClientIP(r), r.UserAgent(), cookieValue(r, cookieDevice))
		if err != nil {
			return err
		}
		if res.SessionToken != "" {
			h.setCookie(w, cookieSession, res.SessionToken, h.S.cfg.SessionMax)
			writeJSON(w, 200, map[string]any{"done": true})
			return nil
		}
		writeJSON(w, 200, map[string]any{"done": false, "ticket": res.Ticket})
		return nil
	}))
	mux.HandleFunc("POST /api/auth/totp", h.open(func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Ticket, Code, DeviceName string
			Trust                    bool
		}
		if err := readJSON(w, r, &in); err != nil {
			return err
		}
		sess, dev, err := h.S.CompleteTOTP(in.Ticket, in.Code, in.Trust, in.DeviceName, h.ClientIP(r), r.UserAgent())
		if err != nil {
			return err
		}
		h.setCookie(w, cookieSession, sess, h.S.cfg.SessionMax)
		if dev != "" {
			h.setCookie(w, cookieDevice, dev, h.S.cfg.TrustDevice)
		}
		writeJSON(w, 200, map[string]any{"done": true})
		return nil
	}))

	mux.Handle("GET /api/me", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		writeJSON(w, 200, map[string]any{"user": viewUser(id.User), "csrf": id.CSRF, "reverified": id.Reverified,
			"pending": append([]string{}, id.Pending...), "server_time": h.S.now()})
		return nil
	}))
	mux.Handle("POST /api/auth/logout", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		h.S.Logout(id)
		h.setCookie(w, cookieSession, "", 0)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	}))
	mux.Handle("POST /api/auth/reverify", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		var in struct{ Code string }
		if err := readJSON(w, r, &in); err != nil {
			return err
		}
		if err := h.S.Reverify(id, in.Code); err != nil {
			return err
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	}))
	mux.Handle("POST /api/me/password", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		var in struct{ Old, New string }
		if err := readJSON(w, r, &in); err != nil {
			return err
		}
		if err := h.S.ChangePassword(id, in.Old, in.New); err != nil {
			return err
		}
		h.setCookie(w, cookieDevice, "", 0) // all trusted devices were just revoked
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	}))
	mux.Handle("POST /api/me/totp/begin", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		secret, uri, err := h.S.TOTPBegin(id)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]string{"secret": secret, "uri": uri})
		return nil
	}))
	mux.Handle("POST /api/me/totp/confirm", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		var in struct{ Code string }
		if err := readJSON(w, r, &in); err != nil {
			return err
		}
		codes, err := h.S.TOTPConfirm(id, in.Code)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"recovery_codes": codes})
		return nil
	}))
	mux.Handle("POST /api/me/recovery-codes", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		codes, err := h.S.RegenerateRecoveryCodes(id)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"recovery_codes": codes})
		return nil
	}))
	mux.Handle("GET /api/me/devices", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		list, err := h.S.ListDevices(id.User.ID)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"devices": list})
		return nil
	}))
	mux.Handle("DELETE /api/me/devices/{id}", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		if err := h.S.RevokeDevice(id, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	}))
	mux.Handle("GET /api/me/sessions", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		list, err := h.S.ListLoginSessions(id)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"sessions": list})
		return nil
	}))
	mux.Handle("DELETE /api/me/sessions/{id}", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		if err := h.S.RevokeLoginSession(id, r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	}))

	// administration
	mux.Handle("GET /api/admin/users", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		users, err := h.S.ListUsers(id)
		if err != nil {
			return err
		}
		out := make([]userView, len(users))
		for i, u := range users {
			out[i] = viewUser(u)
		}
		writeJSON(w, 200, map[string]any{"users": out})
		return nil
	}))
	mux.Handle("POST /api/admin/users", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		var in struct{ Username, DisplayName, Role string }
		if err := readJSON(w, r, &in); err != nil {
			return err
		}
		temp, err := h.S.CreateUser(id, in.Username, in.DisplayName, in.Role)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]string{"temp_password": temp})
		return nil
	}))
	mux.Handle("POST /api/admin/users/{id}/{action}", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		uid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return ErrBadRequest
		}
		out := map[string]any{"ok": true}
		switch r.PathValue("action") {
		case "disable":
			err = h.S.SetStatus(id, uid, "disabled")
		case "enable":
			err = h.S.SetStatus(id, uid, "active")
		case "make-admin":
			err = h.S.SetRole(id, uid, "admin")
		case "make-user":
			err = h.S.SetRole(id, uid, "user")
		case "reset-totp":
			var temp string
			temp, err = h.S.ResetTOTP(id, uid)
			out["temp_password"] = temp
		case "force-logout":
			err = h.S.ForceLogout(id, uid)
		case "reset-password":
			var temp string
			temp, err = h.S.ResetPassword(id, uid)
			out["temp_password"] = temp
		default:
			err = ErrNotFound
		}
		if err != nil {
			return err
		}
		writeJSON(w, 200, out)
		return nil
	}))
	mux.Handle("GET /api/admin/audit", h.authed(func(w http.ResponseWriter, r *http.Request, id *Identity) error {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		list, err := h.S.QueryAudit(id, r.URL.Query().Get("event"), r.URL.Query().Get("username"), limit)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"entries": list})
		return nil
	}))
}
