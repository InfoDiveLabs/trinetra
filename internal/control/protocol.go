package control

import (
	"bufio"
	"encoding/json"
	"io"
)

// ProtocolVersion is the control-socket wire protocol version. The server
// rejects a client hello with a different version.
const ProtocolVersion = 1

// request is one client-to-server call frame. Node, when non-empty, routes the
// call to that fleet node (see resolveNode); it is omitted when empty so a daemon
// that predates fleet routing sees unchanged frames.
type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Node   string          `json:"node,omitempty"`
}

// response is one server-to-client reply frame. When OK is false, Error carries
// the error text. Node echoes request.Node so the client can detect a daemon that
// predates fleet routing: it answers with its own host's data but never echoes
// Node (see Client.call, which treats that as a hard error).
type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Node   string          `json:"node,omitempty"`
}

// streamID is the reserved response.ID for every event frame in Subscribe
// streaming mode. Client.nextID starts at 1, so one-shot calls never see it.
const streamID = -1

// hello is the first frame in each direction. The client's carries Token, the
// per-launch secret; the server's echo leaves it empty.
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
