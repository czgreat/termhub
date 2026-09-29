package proto

import (
	"encoding/binary"
	"io"
)

// Frame types on links B and C, docs/M1 3.2 and 3.3.
const (
	TypeOutput  byte = 0x01 // node -> hub: offset u64 + bytes
	TypeInput   byte = 0x02 // hub -> node: bytes
	TypeControl byte = 0x10 // link C only: JSON control message
	TypeReplay  byte = 0x11 // node -> hub: req u32 + offset u64 + bytes
)

const dataHeader = 1 + 16 // type + session id

// Frame is a decoded link B/C binary frame. Data aliases the input buffer.
type Frame struct {
	Type   byte
	SID    SID
	Offset uint64 // TypeOutput, TypeReplay
	Req    uint32 // TypeReplay
	Data   []byte
}

// AppendFrame encodes f onto dst. For TypeControl, SID is not written.
func AppendFrame(dst []byte, f Frame) ([]byte, error) {
	switch f.Type {
	case TypeOutput:
		if len(f.Data) > MaxOutputData {
			return dst, ErrFrameSize
		}
		dst = append(append(dst, f.Type), f.SID[:]...)
		dst = binary.BigEndian.AppendUint64(dst, f.Offset)
	case TypeInput:
		if len(f.Data) > MaxInputData {
			return dst, ErrFrameSize
		}
		dst = append(append(dst, f.Type), f.SID[:]...)
	case TypeReplay:
		if len(f.Data) > MaxFrame-dataHeader-12 {
			return dst, ErrFrameSize
		}
		dst = append(append(dst, f.Type), f.SID[:]...)
		dst = binary.BigEndian.AppendUint32(dst, f.Req)
		dst = binary.BigEndian.AppendUint64(dst, f.Offset)
	case TypeControl:
		if len(f.Data) > MaxJSON {
			return dst, ErrFrameSize
		}
		dst = append(dst, f.Type)
	default:
		return dst, ErrFrameType
	}
	return append(dst, f.Data...), nil
}

// ParseFrame decodes one link B/C binary frame.
func ParseFrame(b []byte) (Frame, error) {
	var f Frame
	if len(b) > MaxFrame {
		return f, ErrFrameSize
	}
	if len(b) < 1 {
		return f, ErrFrameShort
	}
	f.Type = b[0]
	if f.Type == TypeControl {
		if len(b)-1 > MaxJSON {
			return f, ErrFrameSize
		}
		f.Data = b[1:]
		return f, nil
	}
	if len(b) < dataHeader {
		return f, ErrFrameShort
	}
	copy(f.SID[:], b[1:dataHeader])
	rest := b[dataHeader:]
	switch f.Type {
	case TypeOutput:
		if len(rest) < 8 {
			return f, ErrFrameShort
		}
		f.Offset, f.Data = binary.BigEndian.Uint64(rest), rest[8:]
		if len(f.Data) > MaxOutputData {
			return f, ErrFrameSize
		}
	case TypeInput:
		f.Data = rest
		if len(f.Data) > MaxInputData {
			return f, ErrFrameSize
		}
	case TypeReplay:
		if len(rest) < 12 {
			return f, ErrFrameShort
		}
		f.Req = binary.BigEndian.Uint32(rest)
		f.Offset, f.Data = binary.BigEndian.Uint64(rest[4:]), rest[12:]
	default:
		return f, ErrFrameType
	}
	if f.Offset+uint64(len(f.Data)) < f.Offset {
		return f, ErrFrameShort // offset overflow
	}
	return f, nil
}

// Link A carries one session per connection, so frames have no session id:
// hub -> browser is offset u64 + bytes, browser -> hub is raw input bytes.

// AppendBrowserOutput encodes a hub -> browser frame.
func AppendBrowserOutput(dst []byte, offset uint64, data []byte) ([]byte, error) {
	if len(data) > MaxFrame-8 {
		return dst, ErrFrameSize
	}
	return append(binary.BigEndian.AppendUint64(dst, offset), data...), nil
}

// ParseBrowserOutput decodes a hub -> browser frame.
func ParseBrowserOutput(b []byte) (offset uint64, data []byte, err error) {
	if len(b) > MaxFrame {
		return 0, nil, ErrFrameSize
	}
	if len(b) < 8 {
		return 0, nil, ErrFrameShort
	}
	offset, data = binary.BigEndian.Uint64(b), b[8:]
	if offset+uint64(len(data)) < offset {
		return 0, nil, ErrFrameShort
	}
	return offset, data, nil
}

// CheckBrowserInput validates a browser -> hub input frame.
func CheckBrowserInput(b []byte) error {
	if len(b) > MaxInputData {
		return ErrFrameSize
	}
	return nil
}

// The named pipe of link C is a byte stream, so each frame is preceded by a
// 4-byte big-endian length.

// WritePipeFrame writes one length-prefixed frame.
func WritePipeFrame(w io.Writer, frame []byte) error {
	if len(frame) > MaxFrame {
		return ErrFrameSize
	}
	buf := make([]byte, 4, 4+len(frame))
	binary.BigEndian.PutUint32(buf, uint32(len(frame)))
	_, err := w.Write(append(buf, frame...))
	return err
}

// ReadPipeFrame reads one length-prefixed frame. It refuses an oversized
// length before allocating.
func ReadPipeFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return nil, ErrFrameSize
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}
