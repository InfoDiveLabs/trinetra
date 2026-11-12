package pluginsdk

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const (
	serverHelloTimeout = 10 * time.Second
	serverIdleTimeout  = 2 * time.Minute
)

type connSet struct {
	mu sync.Mutex
	m  map[net.Conn]struct{}
}

func (s *connSet) add(c net.Conn) {
	s.mu.Lock()
	if s.m == nil {
		s.m = map[net.Conn]struct{}{}
	}
	s.m[c] = struct{}{}
	s.mu.Unlock()
}

func (s *connSet) remove(c net.Conn) {
	s.mu.Lock()
	delete(s.m, c)
	s.mu.Unlock()
}

func (s *connSet) closeAll() {
	s.mu.Lock()
	for c := range s.m {
		c.Close()
	}
	s.mu.Unlock()
}

func (p *Plugin) serve(ctx context.Context, ln net.Listener, token string) {
	var conns connSet
	var wg sync.WaitGroup
	go func() {
		<-ctx.Done()
		ln.Close()
		conns.closeAll()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		conns.add(c)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conns.remove(c)
			defer c.Close()
			p.serveConn(ctx, c, token)
		}()
	}
	wg.Wait()
}

func (p *Plugin) serveConn(ctx context.Context, c net.Conn, token string) {
	r := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(serverHelloTimeout))
	var h plugin.Hello
	if err := plugin.ReadFrame(r, &h); err != nil {
		return
	}
	if h.Hello != plugin.PluginHelloMagic || h.Version != plugin.PluginProtocolVersion ||
		token == "" || subtle.ConstantTimeCompare([]byte(h.Token), []byte(token)) != 1 {
		_ = plugin.WriteFrame(c, plugin.Response{Error: "plugin: invalid hello or token"})
		return
	}
	if err := plugin.WriteFrame(c, plugin.Hello{Hello: plugin.PluginHelloMagic, Version: plugin.PluginProtocolVersion}); err != nil {
		return
	}
	for {
		_ = c.SetReadDeadline(time.Now().Add(serverIdleTimeout))
		var req plugin.Request
		if err := plugin.ReadFrame(r, &req); err != nil {
			return
		}
		resp := plugin.Response{ID: req.ID}
		result, err := p.dispatch(ctx, req)
		if err != nil {
			resp.Error = err.Error()
		} else if b, merr := json.Marshal(result); merr != nil {
			resp.Error = merr.Error()
		} else {
			resp.OK, resp.Result = true, b
		}
		_ = c.SetWriteDeadline(time.Now().Add(serverIdleTimeout))
		if err := plugin.WriteFrame(c, resp); err != nil {
			if errors.Is(err, plugin.ErrTooLarge) {
				_ = plugin.WriteFrame(c, plugin.Response{ID: req.ID, Error: plugin.ErrTooLarge.Error()})
				continue
			}
			return
		}
	}
}

func (p *Plugin) dispatch(ctx context.Context, req plugin.Request) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			res, err = nil, fmt.Errorf("plugin handler panic: %v", r)
		}
	}()
	switch req.Method {
	case plugin.CallRenderSlot:
		var sr plugin.SlotRequest
		if err := json.Unmarshal(req.Params, &sr); err != nil {
			return nil, err
		}
		p.mu.Lock()
		f := p.slots[sr.SlotID]
		p.mu.Unlock()
		if f == nil {
			return nil, fmt.Errorf("no handler for slot %q", sr.SlotID)
		}
		cctx, cancel := context.WithTimeout(ctx, plugin.RenderSlotTimeout)
		defer cancel()
		ws, err := f(cctx, sr)
		if err != nil {
			return nil, err
		}
		return plugin.RenderResult{Widgets: resolveLocal(ws, p.Hello().PluginID)}, nil
	case plugin.CallInvokeAction:
		var ar plugin.ActionRequest
		if err := json.Unmarshal(req.Params, &ar); err != nil {
			return nil, err
		}
		p.mu.Lock()
		f := p.actions[ar.Action]
		p.mu.Unlock()
		if f == nil {
			return nil, fmt.Errorf("no handler for action %q", ar.Action)
		}
		cctx, cancel := context.WithTimeout(ctx, plugin.InvokeActionTimeout)
		defer cancel()
		out, err := f(cctx, ar)
		if err != nil {
			return nil, err
		}
		out.Widgets = resolveLocal(out.Widgets, p.Hello().PluginID)
		return out, nil
	case plugin.CallValidateConfig:
		var vr plugin.ValidateConfigRequest
		if err := json.Unmarshal(req.Params, &vr); err != nil {
			return nil, err
		}
		p.mu.Lock()
		f := p.validate
		p.mu.Unlock()
		if f == nil {
			return plugin.ValidateConfigResult{}, nil
		}
		return plugin.ValidateConfigResult{Errors: f(Config{Values: vr.Values})}, nil
	case plugin.CallOnEvent:
		var n plugin.EventNotice
		if err := json.Unmarshal(req.Params, &n); err != nil {
			return nil, err
		}
		p.mu.Lock()
		f := p.event
		p.mu.Unlock()
		if f != nil {
			f(n)
		}
		return struct{}{}, nil
	}
	return nil, fmt.Errorf("unknown method %q", req.Method)
}
