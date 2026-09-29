package history

import (
	"bytes"
	"io"
	"os"
)

// splitLines splits b into non-empty lines without their "\r\n" / "\n".
func splitLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		var ln []byte
		if i < 0 {
			ln, b = b, nil
		} else {
			ln, b = b[:i], b[i+1:]
		}
		ln = bytes.TrimRight(ln, "\r")
		if len(ln) > 0 {
			out = append(out, ln)
		}
	}
	return out
}

// readHeadTail reads the first and last windows of a history file
// (docs/M10 第 3.2 节: never the whole file). Head lines are complete lines
// only; the partial first line of the tail window is skipped. A small file is
// read once and returned as both. firstLine > 0 lets an over-long first line
// be read up to that many bytes (Codex keeps its metadata there).
func readHeadTail(path string, firstLine int) (head, tail [][]byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := st.Size()
	if size <= headSize+tailSize {
		all, err := io.ReadAll(io.LimitReader(f, headSize+tailSize))
		if err != nil {
			return nil, nil, err
		}
		lines := splitLines(all)
		return lines, lines, nil
	}
	buf := make([]byte, headSize)
	n, err := io.ReadFull(f, buf)
	if err != nil {
		return nil, nil, err
	}
	buf = buf[:n]
	if firstLine > headSize && bytes.IndexByte(buf, '\n') < 0 {
		more := make([]byte, firstLine-headSize)
		m, _ := io.ReadFull(f, more)
		buf = append(buf, more[:m]...)
	}
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		head = splitLines(buf[:i])
	}
	tb := make([]byte, tailSize)
	m, err := f.ReadAt(tb, size-tailSize)
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	tb = tb[:m]
	if i := bytes.IndexByte(tb, '\n'); i >= 0 {
		tail = splitLines(tb[i+1:])
	}
	return head, tail, nil
}

// record is what one line contributes to a transcript.
type record struct {
	kind  int // recNone, recUser, recAssistant
	text  string
	tools []string
	time  int64
}

const (
	recNone = iota
	recUser
	recAssistant
)

// turn collects the assistant lines of one turn while reading backwards.
type turn struct {
	parts []record // newest first
}

func (t *turn) empty() bool { return len(t.parts) == 0 }

func (t *turn) message() Message {
	m := Message{Role: "assistant"}
	var texts []string
	seen := map[string]bool{}
	for i := len(t.parts) - 1; i >= 0; i-- {
		r := t.parts[i]
		if m.Time == 0 {
			m.Time = r.time
		}
		if r.text != "" {
			texts = append(texts, r.text)
		}
		for _, n := range r.tools {
			if n != "" && !seen[n] {
				seen[n] = true
				m.Tools = append(m.Tools, n)
			}
		}
	}
	m.Text = capText(joinTexts(texts))
	t.parts = t.parts[:0]
	return m
}

func joinTexts(s []string) string {
	var b bytes.Buffer
	for i, x := range s {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(x)
	}
	return b.String()
}

// readBack reads a history file backwards from before (0 = end of file) in
// readWindow windows aligned to line starts, until limit messages are
// collected or readBudget bytes were read. Assistant lines up to the next
// real user message form one turn, and a page only ends on a turn boundary
// (except at the byte budget). The cursor is the offset of the earliest line
// consumed. Messages come back in chronological order.
func readBack(path string, before int64, limit int, classify func([]byte) record) ([]Message, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	end := st.Size()
	if before > 0 && before < end {
		end = before
	}
	var (
		rev    []Message // newest first
		pend   turn
		pendAt int64  // offset of the oldest line of the turn being collected
		carry  []byte // start of a line whose beginning is before pos
		pos    = end
		cursor = end
		read   int64
		done   bool
	)
	for pos > 0 && !done {
		start := pos - readWindow
		if start < 0 {
			start = 0
		}
		buf := make([]byte, pos-start, pos-start+int64(len(carry)))
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, 0, err
		}
		buf = append(buf, carry...)
		read += pos - start
		pos = start
		body, off := buf, start
		if start > 0 {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 { // one line longer than the window: keep reading
				carry = buf
				if read >= readBudget {
					break
				}
				continue
			}
			carry, body, off = buf[:i+1], buf[i+1:], start+int64(i+1)
		} else {
			carry = nil
		}
		// Line start offsets inside body, in file order.
		var starts []int
		for i := 0; i < len(body); {
			starts = append(starts, i)
			j := bytes.IndexByte(body[i:], '\n')
			if j < 0 {
				break
			}
			i += j + 1
		}
		for n := len(starts) - 1; n >= 0 && !done; n-- {
			s := starts[n]
			e := len(body)
			if n+1 < len(starts) {
				e = starts[n+1]
			}
			ln := bytes.TrimRight(body[s:e], "\r\n")
			r := record{}
			if len(ln) > 0 {
				r = classify(ln)
			}
			switch r.kind {
			case recAssistant:
				pend.parts = append(pend.parts, r)
				pendAt = off + int64(s)
			case recUser:
				if !pend.empty() {
					m := pend.message()
					m.start = pendAt
					rev = append(rev, m)
					if len(rev) >= limit { // the user line stays for the next page
						cursor = off + int64(e)
						done = true
						continue
					}
				}
				rev = append(rev, Message{Role: "user", Text: capText(r.text), Time: r.time, start: off + int64(s)})
				if len(rev) >= limit {
					done = true
				}
			}
			cursor = off + int64(s)
		}
		if read >= readBudget {
			break
		}
	}
	if !pend.empty() {
		m := pend.message()
		m.start = pendAt
		rev = append(rev, m)
	}
	if cursor == end && pos < end && !done { // budget spent inside one huge line
		cursor = pos
	}
	msgs := make([]Message, len(rev))
	for i, m := range rev {
		msgs[len(rev)-1-i] = m
	}
	return msgs, cursor, nil
}
