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
// expect a matching response frame carrying the same ID.
type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// response is one server-to-client reply frame. When OK is false, Error
// carries the method's error text and Result is empty.
type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

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
