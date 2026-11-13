package pluginsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const hostCallTimeout = 15 * time.Second

type hostConn struct {
	path, token string
	mu          sync.Mutex
	conn        net.Conn
	r           *bufio.Reader
	next        int
}

type hostReply struct {
	Hello   string          `json:"hello"`
	Version int             `json:"version"`
	ID      int             `json:"id"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result"`
	Error   string          `json:"error"`
}

func handshake(ctx context.Context, path, token string) (net.Conn, *bufio.Reader, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(hostCallTimeout))
	if err := plugin.WriteFrame(conn, plugin.Hello{Hello: plugin.ControlHelloMagic, Version: plugin.ControlProtocolVersion, Token: token}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	r := bufio.NewReader(conn)
	var rep hostReply
	if err := plugin.ReadFrame(r, &rep); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if rep.Hello != plugin.ControlHelloMagic {
		conn.Close()
		if rep.Error != "" {
			return nil, nil, errors.New(rep.Error)
		}
		return nil, nil, fmt.Errorf("unexpected host hello %q", rep.Hello)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, r, nil
}

func (h *hostConn) close() {
	if h.conn != nil {
		h.conn.Close()
		h.conn, h.r = nil, nil
	}
}

func (h *hostConn) call(method string, params, result any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		reused := h.conn != nil
		if h.conn == nil {
			ctx, cancel := context.WithTimeout(context.Background(), hostCallTimeout)
			conn, r, err := handshake(ctx, h.path, h.token)
			cancel()
			if err != nil {
				return err
			}
			h.conn, h.r = conn, r
		}
		h.next++
		_ = h.conn.SetDeadline(time.Now().Add(hostCallTimeout))
		var rep hostReply
		err := plugin.WriteFrame(h.conn, plugin.Request{ID: h.next, Method: method, Params: raw})
		if err == nil {
			err = plugin.ReadFrame(h.r, &rep)
		}
		if err != nil {
			h.close()
			if reused && attempt == 0 && !errors.Is(err, plugin.ErrTooLarge) {
				continue
			}
			return err
		}
		if !rep.OK {
			if rep.Error == "" {
				return errors.New("host call failed")
			}
			return errors.New(rep.Error)
		}
		if result != nil && len(rep.Result) > 0 {
			return json.Unmarshal(rep.Result, result)
		}
		return nil
	}
}

func (h *hostConn) closeAll() {
	h.mu.Lock()
	h.close()
	h.mu.Unlock()
}

func watchConfig(ctx context.Context, path, token string, onCfg func(plugin.ConfigResponse)) error {
	conn, r, err := handshake(ctx, path, token)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()
	if err := plugin.WriteFrame(conn, plugin.Request{ID: 1, Method: plugin.MethodWatchConfig}); err != nil {
		return err
	}
	var ack hostReply
	if err := plugin.ReadFrame(r, &ack); err != nil {
		return err
	}
	if !ack.OK {
		return errors.New(ack.Error)
	}
	for {
		var rep hostReply
		if err := plugin.ReadFrame(r, &rep); err != nil {
			return err
		}
		if rep.ID != plugin.StreamID || !rep.OK {
			continue
		}
		var cfg plugin.ConfigResponse
		if json.Unmarshal(rep.Result, &cfg) == nil {
			onCfg(cfg)
		}
	}
}
