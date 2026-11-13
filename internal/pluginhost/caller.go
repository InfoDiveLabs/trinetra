package pluginhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type pstate struct {
	sem     chan struct{}
	mu      sync.Mutex
	queue   []plugin.EventNotice
	pending int
	wake    chan struct{}
	stop    chan struct{}
	started bool
	dropped atomic.Int64
}

// Caller makes core-to-plugin calls over each plugin's own socket.
type Caller struct {
	inventory func() plugin.Inventory
	mu        sync.Mutex
	plugins   map[string]*pstate
}

// NewCaller returns a Caller resolving endpoints through inv.
func NewCaller(inv func() plugin.Inventory) *Caller {
	return &Caller{inventory: inv, plugins: map[string]*pstate{}}
}

func (c *Caller) state(id string) *pstate {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.plugins[id]
	if s == nil {
		s = &pstate{sem: make(chan struct{}, plugin.MaxInflightCalls), wake: make(chan struct{}, 1), stop: make(chan struct{})}
		c.plugins[id] = s
	}
	return s
}

type callReply struct {
	Hello   string          `json:"hello"`
	Version int             `json:"version"`
	OK      bool            `json:"ok"`
	Result  json.RawMessage `json:"result"`
	Error   string          `json:"error"`
}

// Call dials id's socket, says hello with its token, sends one request and decodes the reply within timeout.
func (c *Caller) Call(ctx context.Context, id, method string, params, result any, timeout time.Duration) error {
	var inv plugin.Inventory
	if c.inventory != nil {
		inv = c.inventory()
	}
	if inv == nil {
		return plugin.ErrNotRunning
	}
	sock, tok, ok := inv.Endpoint(id)
	if !ok || sock == "" {
		return fmt.Errorf("%w: %s", plugin.ErrNotRunning, id)
	}
	s := c.state(id)
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return fmt.Errorf("%w: %s", plugin.ErrBusy, id)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := c.roundTrip(ctx, sock, tok, method, params, result)
	if err != nil && (errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)) && !errors.Is(err, plugin.ErrTooLarge) {
		return fmt.Errorf("%w: %s.%s after %v", plugin.ErrDeadline, id, method, timeout)
	}
	return err
}

func (c *Caller) roundTrip(ctx context.Context, sock, tok, method string, params, result any) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return fmt.Errorf("%w: %v", plugin.ErrNotRunning, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Unix(1, 0))
		case <-done:
		}
	}()
	r := bufio.NewReader(conn)
	if err := plugin.WriteFrame(conn, plugin.Hello{Hello: plugin.PluginHelloMagic, Version: plugin.PluginProtocolVersion, Token: tok}); err != nil {
		return err
	}
	var hr callReply
	if err := plugin.ReadFrame(r, &hr); err != nil {
		return err
	}
	if hr.Hello != plugin.PluginHelloMagic || hr.Version != plugin.PluginProtocolVersion {
		if hr.Error != "" {
			return fmt.Errorf("plugin refused hello: %s", hr.Error)
		}
		return fmt.Errorf("unexpected plugin hello %q version %d", hr.Hello, hr.Version)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err := plugin.WriteFrame(conn, plugin.Request{ID: 1, Method: method, Params: raw}); err != nil {
		return err
	}
	var resp callReply
	if err := plugin.ReadFrame(r, &resp); err != nil {
		return err
	}
	if resp.Error != "" || !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "plugin call failed"
		}
		return errors.New(msg)
	}
	if result != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, result)
	}
	return nil
}

// RenderSlot asks id to render a slot.
func (c *Caller) RenderSlot(ctx context.Context, id string, req plugin.SlotRequest) (plugin.RenderResult, error) {
	var out plugin.RenderResult
	err := c.Call(ctx, id, plugin.CallRenderSlot, req, &out, plugin.RenderSlotTimeout)
	return out, err
}

// InvokeAction runs one of id's actions.
func (c *Caller) InvokeAction(ctx context.Context, id string, req plugin.ActionRequest) (plugin.ActionResult, error) {
	var out plugin.ActionResult
	err := c.Call(ctx, id, plugin.CallInvokeAction, req, &out, plugin.InvokeActionTimeout)
	return out, err
}

// ValidateConfig asks id to validate proposed config values.
func (c *Caller) ValidateConfig(ctx context.Context, id string, values map[string]json.RawMessage) (plugin.FieldErrors, error) {
	var out plugin.ValidateConfigResult
	if err := c.Call(ctx, id, plugin.CallValidateConfig, plugin.ValidateConfigRequest{Values: values}, &out, plugin.ValidateConfigTimeout); err != nil {
		return nil, err
	}
	if len(out.Errors) == 0 {
		return nil, nil
	}
	return plugin.FieldErrors(out.Errors), nil
}

// Event enqueues a notice for id, dropping it when the queue is full; it never blocks.
func (c *Caller) Event(id string, n plugin.EventNotice) {
	s := c.state(id)
	s.mu.Lock()
	if s.pending >= plugin.EventQueueSize {
		s.mu.Unlock()
		s.dropped.Add(1)
		return
	}
	s.pending++
	s.queue = append(s.queue, n)
	if !s.started {
		s.started = true
		go c.pump(id, s)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (c *Caller) pump(id string, s *pstate) {
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.stop:
				return
			}
		}
		n := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		select {
		case <-s.stop:
			return
		default:
		}
		_ = c.Call(context.Background(), id, plugin.CallOnEvent, n, nil, plugin.OnEventTimeout)
		s.mu.Lock()
		s.pending--
		s.mu.Unlock()
	}
}

// EventsDropped returns how many events for id were dropped on a full queue.
func (c *Caller) EventsDropped(id string) int64 { return c.state(id).dropped.Load() }

// Stop discards id's queued events and ends its delivery goroutine.
func (c *Caller) Stop(id string) {
	c.mu.Lock()
	s := c.plugins[id]
	delete(c.plugins, id)
	c.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.queue, s.pending = nil, 0
	s.mu.Unlock()
	close(s.stop)
}
