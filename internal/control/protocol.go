package control

import (
	"bufio"
	"encoding/json"
	"io"
)

// ProtocolVersion is the control-socket wire protocol version.
const ProtocolVersion = 1

// request is one client-to-server call frame.
type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Node   string          `json:"node,omitempty"`
}

// response is one server-to-client reply frame.
type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Node   string          `json:"node,omitempty"`
}

// streamID is the reserved response.ID for every event frame in Subscribe streaming mode.
const streamID = -1

// hello is the first frame in each direction.
type hello struct {
	Hello   string `json:"hello"`
	Version int    `json:"version"`
	Token   string `json:"token,omitempty"`
}

// enrollmentPINResult is the Result payload for EnrollmentPIN.
type enrollmentPINResult struct {
	PIN      string `json:"pin"`
	Enrolled bool   `json:"enrolled"`
}

// writeFrame writes v as one JSON line so peers can delimit frames by newline.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// readFrame reads one newline-delimited JSON frame into v, or returns io.EOF.
func readFrame(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return err
	}
	return json.Unmarshal(line, v)
}
