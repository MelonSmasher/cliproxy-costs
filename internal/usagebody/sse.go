package usagebody

import "bytes"

// Frame is one SSE event inside a chunk. Offsets index the original chunk.
type Frame struct {
	Event     string
	Data      []byte // data lines joined with "\n" (per the SSE spec)
	DataStart int    // start of the single data value; -1 when DataLines != 1
	DataEnd   int
	DataLines int
}

// ParseFrames splits a chunk into SSE events. Line endings may be LF, CRLF
// or CR. A trailing event without a terminating blank line is still reported
// (CPA delivers one event per chunk and may omit the final separator).
func ParseFrames(chunk []byte) []Frame {
	var frames []Frame
	cur := Frame{DataStart: -1}
	flush := func() {
		if cur.Event != "" || cur.DataLines > 0 {
			if cur.DataLines != 1 {
				cur.DataStart, cur.DataEnd = -1, -1
			}
			frames = append(frames, cur)
		}
		cur = Frame{DataStart: -1}
	}
	i := 0
	for i < len(chunk) {
		end := i
		for end < len(chunk) && chunk[end] != '\n' && chunk[end] != '\r' {
			end++
		}
		line := chunk[i:end]
		next := end
		if next < len(chunk) {
			if chunk[next] == '\r' && next+1 < len(chunk) && chunk[next+1] == '\n' {
				next += 2
			} else {
				next++
			}
		}
		switch {
		case len(line) == 0:
			flush()
		case line[0] == ':':
			// comment
		default:
			field, value, valueStart := line, []byte(nil), end
			if c := bytes.IndexByte(line, ':'); c >= 0 {
				field = line[:c]
				value = line[c+1:]
				valueStart = i + c + 1
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
					valueStart++
				}
			}
			switch string(field) {
			case "event":
				cur.Event = string(value)
			case "data":
				if cur.DataLines > 0 {
					cur.Data = append(cur.Data, '\n')
				}
				cur.Data = append(cur.Data, value...)
				cur.DataLines++
				cur.DataStart, cur.DataEnd = valueStart, end
			}
		}
		i = next
	}
	flush()
	return frames
}

// Splice replaces the data value of a single-line frame, returning a new chunk.
func Splice(chunk []byte, f Frame, data []byte) ([]byte, bool) {
	if f.DataStart < 0 || f.DataEnd < f.DataStart || f.DataEnd > len(chunk) || bytes.ContainsAny(data, "\r\n") {
		return chunk, false
	}
	out := make([]byte, 0, len(chunk)-(f.DataEnd-f.DataStart)+len(data))
	out = append(out, chunk[:f.DataStart]...)
	out = append(out, data...)
	out = append(out, chunk[f.DataEnd:]...)
	return out, true
}
