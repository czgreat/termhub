package proto

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Control message types, docs/M1 第 5 节.
const (
	// generic
	MsgOK   = "ok"
	MsgErr  = "err"
	MsgPing = "ping"
	MsgPong = "pong"

	// link A, /ws/session/{sid}
	MsgInputBatch  = "input_batch" // browser-only acknowledged queue admission
	MsgInputResult = "input_result"
	MsgAttach      = "attach"
	MsgAttached    = "attached"
	MsgResize      = "resize"
	MsgRedraw      = "redraw"
	MsgSize        = "size"
	MsgPeers       = "peers"
	MsgGap         = "gap"
	MsgExited      = "exited"
	MsgNodeOffline = "node_offline"
	MsgNodeOnline  = "node_online"
	MsgDriver      = "driver" // who types into the session: on = this viewer may, by = the administrator who took it over

	// link A, /ws/events
	MsgSessionsChanged = "sessions_changed"
	MsgNodeStatus      = "node_status"
	MsgSessionExited   = "session_exited"
	MsgAuthExpired     = "auth_expired"
	MsgAgentUpgraded   = "agent_upgraded"

	// links B and C
	MsgHello         = "hello"
	MsgWelcome       = "welcome"
	MsgSessions      = "sessions"
	MsgCreate        = "create"
	MsgForward       = "forward"
	MsgReplay        = "replay"
	MsgReplayBegin   = "replay_begin"
	MsgReplayEnd     = "replay_end"
	MsgClose         = "close"
	MsgTitle         = "title"
	MsgUpgrade       = "upgrade"
	MsgUpgradeStatus = "upgrade_status"
	MsgFsList        = "fs_list"
	MsgFsMkdir       = "fs_mkdir"
	MsgFsStat        = "fs_stat"
	MsgClipboard     = "clipboard" // Hub → node: put an uploaded image on the session's clipboard (docs/M4 第 7 节)
	MsgTransferOpen  = "transfer_open"
	MsgHistoryList   = "history_list"
	MsgHistoryRead   = "history_read" // one conversation's messages (docs/M10 第 5 节)
	MsgUsage         = "usage"        // what a running session's conversation used (主人 09-26: 美元与上下文)

	// link C only
	MsgLink     = "link"
	MsgShutdown = "shutdown"
)

// Attach modes.
const (
	ModeResume = "resume"
	ModeReplay = "replay"
)

// Close modes.
const (
	CloseGraceful = "graceful"
	CloseForce    = "force"
)

// Profile is the snapshot of a CLI profile sent with create. The node never
// looks anything up in the Hub's database (docs/M1 5.4).
type Profile struct {
	Name        string            `json:"name,omitempty"`
	Kind        string            `json:"kind,omitempty"` // claude, codex, shell, custom
	Mode        string            `json:"mode"`           // shell or direct
	ShellPath   string            `json:"shell_path,omitempty"`
	ShellArgs   []string          `json:"shell_args,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	IdleTimeout int               `json:"idle_timeout,omitempty"` // seconds, 0 = never
	BufferBytes int               `json:"buffer_bytes,omitempty"`
}

// SessionInfo is one entry of a sessions list.
type SessionInfo struct {
	Kind          string   `json:"kind,omitempty"` // creation snapshot, not the profile's current kind
	SID           SID      `json:"sid"`
	Profile       string   `json:"profile,omitempty"`
	Owner         string   `json:"owner,omitempty"`
	Cwd           string   `json:"cwd,omitempty"`
	State         string   `json:"state"`
	Cols          int      `json:"cols"`
	Rows          int      `json:"rows"`
	Base          uint64   `json:"base"`
	End           uint64   `json:"end"`
	Created       int64    `json:"created"` // unix seconds
	Title         string   `json:"title,omitempty"`
	JobEscapeRisk bool     `json:"job_escape_risk,omitempty"`
	IdleDeadline  int64    `json:"idle_deadline,omitempty"`
	Flags         []string `json:"flags,omitempty"` // e.g. codepage_not_set
}

// Msg is the flat envelope of every control message. Which fields are
// meaningful depends on T. Unknown JSON fields are ignored on decode, so minor
// versions can add fields freely (docs/M1 5.1, 第 9 节).
type Msg struct {
	InputAck bool   `json:"input_ack,omitempty"`
	Data     []byte `json:"data,omitempty"`
	T        string `json:"t"`
	ID       uint64 `json:"id,omitempty"` // set on requests; the peer must answer with Re
	Re       uint64 `json:"re,omitempty"`
	SID      *SID   `json:"sid,omitempty"`

	Code string `json:"code,omitempty"` // error code on err
	Text string `json:"msg,omitempty"`

	// offsets. Have and From are pointers because null ("nothing yet, replay
	// everything") differs from 0.
	Have *uint64 `json:"have,omitempty"`
	From *uint64 `json:"from,omitempty"`
	Next uint64  `json:"next,omitempty"`
	End  uint64  `json:"end,omitempty"`
	Req  uint32  `json:"req,omitempty"`

	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	Mode  string `json:"mode,omitempty"`
	By    string `json:"by,omitempty"`
	Count int    `json:"count,omitempty"`
	On    *bool  `json:"on,omitempty"`
	Up    *bool  `json:"up,omitempty"`
	Force bool   `json:"force,omitempty"`

	Prelude   []byte `json:"prelude,omitempty"`    // replay: mode-setting sequences, base64 in JSON
	AltScreen bool   `json:"alt_screen,omitempty"` // replay: session is on the alternate screen

	Reason   string   `json:"reason,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Leftover []string `json:"leftover,omitempty"`
	Title    string   `json:"title,omitempty"`

	Proto       string         `json:"proto,omitempty"`
	AgentVer    string         `json:"agent_ver,omitempty"`
	HostVer     string         `json:"host_ver,omitempty"`
	Name        string         `json:"name,omitempty"`
	OSBuild     string         `json:"os_build,omitempty"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	Caps        []string       `json:"caps,omitempty"`
	PwshStore   bool           `json:"pwsh_store,omitempty"`
	NodeID      string         `json:"node_id,omitempty"`
	Node        string         `json:"node,omitempty"`
	Online      *bool          `json:"online,omitempty"`
	Settings    map[string]any `json:"settings,omitempty"`

	List    []SessionInfo `json:"list,omitempty"`
	Owner   string        `json:"owner,omitempty"`
	Profile *Profile      `json:"profile,omitempty"`
	Cwd     string        `json:"cwd,omitempty"`

	Ticket string `json:"ticket,omitempty"`
	Dir    string `json:"dir,omitempty"` // up or down
	Path   string `json:"path,omitempty"`
	Size   int64  `json:"size,omitempty"`

	Version string `json:"version,omitempty"`
	URL     string `json:"url,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Sig     string `json:"sig,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Error   string `json:"error,omitempty"`

	// Payload carries results of requests whose shape belongs to another
	// module (fs_*, history_list).
	Payload json.RawMessage `json:"payload,omitempty"`
}

var ErrMsgInvalid = errors.New("proto: invalid control message")

// EncodeMsg serialises m, refusing anything over MaxJSON.
func EncodeMsg(m *Msg) ([]byte, error) {
	if m.T == "" {
		return nil, ErrMsgInvalid
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxJSON {
		return nil, ErrFrameSize
	}
	return b, nil
}

// DecodeMsg parses one control message and validates the fields every
// receiver relies on: the type, and terminal sizes when present.
func DecodeMsg(b []byte) (*Msg, error) {
	if len(b) > MaxJSON {
		return nil, ErrFrameSize
	}
	var m Msg
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMsgInvalid, err)
	}
	if m.T == "" {
		return nil, ErrMsgInvalid
	}
	if m.Cols != 0 || m.Rows != 0 {
		if err := CheckSize(m.Cols, m.Rows); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// Reply builds the ok answer to a request.
func (m *Msg) Reply() *Msg { return &Msg{T: MsgOK, Re: m.ID, SID: m.SID} }

// Fail builds the err answer to a request.
func (m *Msg) Fail(code, text string) *Msg {
	return &Msg{T: MsgErr, Re: m.ID, SID: m.SID, Code: code, Text: text}
}

// IsRequest reports whether the peer expects an answer.
func (m *Msg) IsRequest() bool { return m.ID != 0 }

// Version is a protocol version, docs/M1 第 9 节.
type Version struct{ Major, Minor int }

// Current is the version this build speaks. SupportedMajors lists every major
// it still accepts from a peer: the current one and the previous one.
var (
	Current         = Version{1, 0}
	SupportedMajors = []int{1}
)

func (v Version) String() string { return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) }

func ParseVersion(s string) (Version, error) {
	a, b, ok := strings.Cut(s, ".")
	maj, err1 := strconv.Atoi(a)
	mnr, err2 := strconv.Atoi(b)
	if !ok || err1 != nil || err2 != nil || maj < 1 || mnr < 0 {
		return Version{}, fmt.Errorf("proto: bad version %q", s)
	}
	return Version{maj, mnr}, nil
}

// Negotiate picks the version both sides will use, or fails with
// ErrProtoMismatch. Within a major, the lower minor wins because minors only
// add messages and fields.
func Negotiate(remote Version) (Version, error) {
	for _, maj := range SupportedMajors {
		if maj != remote.Major {
			continue
		}
		if maj == Current.Major {
			return Version{maj, min(Current.Minor, remote.Minor)}, nil
		}
		return remote, nil
	}
	return Version{}, errors.New(ErrProtoMismatch)
}
