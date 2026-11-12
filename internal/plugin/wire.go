package plugin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

const (
	MaxMessageBytes        = 1 << 20
	ControlHelloMagic      = "serverwatch-control"
	ControlProtocolVersion = 1
	PluginHelloMagic       = "trinetra-plugin"
	PluginProtocolVersion  = 1
	StreamID               = -1
	ManifestSigPrefix      = "trinetra-plugin-v1\n"
	IndexSigPrefix         = "trinetra-plugin-index-v1\n"
)

// Env vars set on every plugin process.
const (
	EnvPluginID     = "TRINETRA_PLUGIN_ID"
	EnvHostSocket   = "TRINETRA_PLUGIN_HOST_SOCKET"
	EnvPluginSocket = "TRINETRA_PLUGIN_SOCKET"
	EnvTokenFile    = "TRINETRA_PLUGIN_TOKEN_FILE"
	EnvDataDir      = "TRINETRA_PLUGIN_DATA_DIR"
	EnvDev          = "TRINETRA_PLUGIN_DEV"
)

// Request is one call frame (JSON-identical to internal/control's request).
type Request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Node   string          `json:"node,omitempty"`
}

// Response is one reply frame (JSON-identical to internal/control's response).
type Response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Node   string          `json:"node,omitempty"`
}

// Hello is the first frame in each direction.
type Hello struct {
	Hello   string `json:"hello"`
	Version int    `json:"version"`
	Token   string `json:"token,omitempty"`
}

// WriteFrame writes v as one JSON line; ErrTooLarge if the encoded line exceeds MaxMessageBytes.
func WriteFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b)+1 > MaxMessageBytes {
		return ErrTooLarge
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// ReadFrame reads one line of at most MaxMessageBytes into v; ErrTooLarge (and the rest of the line unread) beyond that.
func ReadFrame(r *bufio.Reader, v any) error {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxMessageBytes {
			return ErrTooLarge
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil && len(buf) == 0 {
			return err
		}
		break
	}
	return json.Unmarshal(bytes.TrimSpace(buf), v)
}
