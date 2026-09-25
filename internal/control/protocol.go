package control

import (
	"bufio"
	"encoding/json"
	"io"
)

// ProtocolVersion is the current control-socket wire protocol version. The
// client sends it in its hello frame; the server rejects a mismatched
// version with an error frame and closes the connection.
const ProtocolVersion = 1

// request is one client-to-server call frame: invoke Method with Params and
// expect a matching response frame carrying the same ID. Node, when
// non-empty, asks the server to route the call to that fleet node instead of
// serving it locally (see server.go's resolveNode); it is omitted entirely
// when empty so an old daemon that doesn't know about fleet routing sees no
// change to the frames it already understands.
type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Node   string          `json:"node,omitempty"`
}

// response is one server-to-client reply frame. When OK is false, Error
// carries the method's error text and Result is empty. Node echoes the
// request's Node field so the client can detect a daemon that doesn't
// understand fleet routing: such a daemon still answers (with its own
// host's data) but never echoes Node, since it has no idea the field exists
// (see client.go's call, which treats a missing echo as a hard error rather
// than silently returning the wrong node's data).
type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Node   string          `json:"node,omitempty"`
}

// streamID is the reserved response.ID for every event frame a connection
// in Subscribe streaming mode sends (server.go's streamSubscribe):
// response{ID: streamID, OK: true, Result: <core.Event JSON>}. Client.call's
// own request ids (Client.nextID) start at 1 and only increase, so a normal
// one-shot call never sees a response carrying streamID; only the client's
// dedicated stream reader (client.go's Subscribe) looks for it.
const streamID = -1

// hello is the first frame exchanged in each direction after a connection
// is established, used to agree on ProtocolVersion before any request or
// response frame is sent. The client's hello also carries Token, the
// per-launch secret proving it is allowed to use this control socket; the
// server's own hello (echoed back once the client's is accepted) leaves
// Token empty.
type hello struct {
	Hello   string `json:"hello"`
	Version int    `json:"version"`
	Token   string `json:"token,omitempty"`
}

// enrollmentPINResult is the Result payload for the EnrollmentPIN method,
// shared by dispatch (server.go, which marshals it) and Client.EnrollmentPIN
// (client.go, which unmarshals it) so the two sides agree on field names.
type enrollmentPINResult struct {
	PIN      string `json:"pin"`
	Enrolled bool   `json:"enrolled"`
}

// writeFrame marshals v to JSON and writes it to w as a single line,
// terminated with a newline so the peer can delimit frames with a
// bufio.Reader or bufio.Scanner.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// readFrame reads one newline-delimited JSON frame from r and unmarshals it
// into v. It returns io.EOF (or the underlying read error) if no frame is
// available.
func readFrame(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return err
	}
	return json.Unmarshal(line, v)
}
