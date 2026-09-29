package proto

// Transfer is the control message of a file transfer connection
// (/node/transfer/{ticket}, docs/M1 第 8 节). File data travels as binary
// messages of at most MaxChunk bytes between these.
type Transfer struct {
	T      string `json:"t"` // ready, ack, finish, cancel, meta, done, err
	Code   string `json:"code,omitempty"`
	Msg    string `json:"msg,omitempty"`
	Next   int64  `json:"next,omitempty"` // ack: offset the next chunk must start at
	Path   string `json:"path,omitempty"` // ready/done: where the file is or will be
	Name   string `json:"name,omitempty"` // meta: file name for the browser
	Size   int64  `json:"size,omitempty"` // meta
	SHA256 string `json:"sha256,omitempty"`
}

const (
	XferReady  = "ready"
	XferAck    = "ack"
	XferFinish = "finish"
	XferCancel = "cancel"
	XferMeta   = "meta"
	XferDone   = "done"
	XferErr    = "err"

	DirUp    = "up"
	DirDown  = "down"
	DirPaste = "paste" // an upload into the session's paste folder; the node picks the path
	DirZip   = "zip"   // a download of a whole folder as a zip stream of unknown length
)
