package pluginhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakePlugin struct {
	sock     string
	tok      string
	handler  func(req plugin.Request) (any, string)
	calls    atomic.Int64
	hold     chan struct{}
	badHello bool
	bigReply bool
}

func startFakePlugin(t *testing.T, inv *fakeInv, id string) *fakePlugin {
	t.Helper()
	fp := &fakePlugin{sock: filepath.Join(sockDir(t), "p.sock"), tok: "secret-token", hold: make(chan struct{})}
	ln, err := net.Listen("unix", fp.sock)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		select {
		case <-fp.hold:
		default:
			close(fp.hold)
		}
		ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				fp.serve(conn)
			}()
		}
	}()
	inv.setEndpoint(id, fp.sock, fp.tok)
	return fp
}

func (fp *fakePlugin) serve(conn net.Conn) {
	r := bufio.NewReader(conn)
	var h plugin.Hello
	if plugin.ReadFrame(r, &h) != nil {
		return
	}
	if h.Token != fp.tok {
		_ = plugin.WriteFrame(conn, plugin.Response{Error: "bad token"})
		return
	}
	if fp.badHello {
		_ = plugin.WriteFrame(conn, plugin.Hello{Hello: "not-trinetra", Version: 1})
		return
	}
	_ = plugin.WriteFrame(conn, plugin.Hello{Hello: plugin.PluginHelloMagic, Version: plugin.PluginProtocolVersion})
	var req plugin.Request
	if plugin.ReadFrame(r, &req) != nil {
		return
	}
	fp.calls.Add(1)
	if fp.bigReply {
		_, _ = conn.Write([]byte(`{"id":1,"ok":true,"result":"` + strings.Repeat("x", plugin.MaxMessageBytes) + `"}` + "\n"))
		return
	}
	var res any = map[string]any{}
	errMsg := ""
	if fp.handler != nil {
		res, errMsg = fp.handler(req)
	}
	if errMsg != "" {
		_ = plugin.WriteFrame(conn, plugin.Response{ID: req.ID, Error: errMsg})
		return
	}
	b, _ := json.Marshal(res)
	_ = plugin.WriteFrame(conn, plugin.Response{ID: req.ID, OK: true, Result: b})
}

func callerRig(t *testing.T) (*Caller, *fakeInv, *fakePlugin) {
	inv := newFakeInv()
	fp := startFakePlugin(t, inv, "probe")
	return NewCaller(func() plugin.Inventory { return inv }), inv, fp
}

func TestCallerRenderSlot(t *testing.T) {
	c, _, fp := callerRig(t)
	fp.handler = func(req plugin.Request) (any, string) {
		var sr plugin.SlotRequest
		_ = json.Unmarshal(req.Params, &sr)
		return plugin.RenderResult{Widgets: []plugin.Widget{{Kind: plugin.WidgetText, Title: sr.SlotID}}}, ""
	}
	out, err := c.RenderSlot(context.Background(), "probe", plugin.SlotRequest{Slot: plugin.SlotNavPage, SlotID: "main"})
	if err != nil || len(out.Widgets) != 1 || out.Widgets[0].Title != "main" {
		t.Fatalf("%+v %v", out, err)
	}
	fp.handler = func(plugin.Request) (any, string) { return nil, "boom" }
	if _, err := c.InvokeAction(context.Background(), "probe", plugin.ActionRequest{Action: "x"}); err == nil || err.Error() != "boom" {
		t.Fatalf("%v", err)
	}
	fp.handler = func(plugin.Request) (any, string) {
		return plugin.ValidateConfigResult{Errors: map[string]string{"url": "bad"}}, ""
	}
	fe, err := c.ValidateConfig(context.Background(), "probe", map[string]json.RawMessage{"url": json.RawMessage(`"x"`)})
	if err != nil || fe["url"] != "bad" {
		t.Fatalf("%v %v", fe, err)
	}
}

func TestCallerRenderSlotDeadline(t *testing.T) {
	c, _, fp := callerRig(t)
	fp.handler = func(plugin.Request) (any, string) {
		<-fp.hold
		return nil, ""
	}
	start := time.Now()
	err := c.Call(context.Background(), "probe", plugin.CallRenderSlot, plugin.SlotRequest{}, nil, 200*time.Millisecond)
	if !errors.Is(err, plugin.ErrDeadline) {
		t.Fatalf("%v", err)
	}
	if el := time.Since(start); el > 400*time.Millisecond {
		t.Fatalf("took %v", el)
	}
}

func TestCallerRejectsOversizedResponse(t *testing.T) {
	c, _, fp := callerRig(t)
	fp.bigReply = true
	err := c.Call(context.Background(), "probe", plugin.CallRenderSlot, plugin.SlotRequest{}, nil, 2*time.Second)
	if !errors.Is(err, plugin.ErrTooLarge) {
		t.Fatalf("%v", err)
	}
}

func TestCallerBusyWhenInflightFull(t *testing.T) {
	c, _, fp := callerRig(t)
	fp.handler = func(plugin.Request) (any, string) {
		<-fp.hold
		return nil, ""
	}
	var wg sync.WaitGroup
	for i := 0; i < plugin.MaxInflightCalls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Call(context.Background(), "probe", plugin.CallRenderSlot, plugin.SlotRequest{}, nil, 3*time.Second)
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for fp.calls.Load() < int64(plugin.MaxInflightCalls) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	err := c.Call(context.Background(), "probe", plugin.CallRenderSlot, plugin.SlotRequest{}, nil, time.Second)
	if !errors.Is(err, plugin.ErrBusy) {
		t.Fatalf("%v", err)
	}
	close(fp.hold)
	wg.Wait()
	if err := c.Call(context.Background(), "probe", plugin.CallRenderSlot, plugin.SlotRequest{}, nil, time.Second); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestOnEventDropsWhenQueueFull(t *testing.T) {
	c, _, fp := callerRig(t)
	fp.handler = func(plugin.Request) (any, string) {
		<-fp.hold
		return nil, ""
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		c.Event("probe", plugin.EventNotice{Kind: plugin.EventAlertFired, Time: int64(i)})
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("Event blocked")
	}
	if d := c.EventsDropped("probe"); d < 36 {
		t.Fatalf("dropped %d", d)
	}
	c.Stop("probe")
}

func TestEventDelivered(t *testing.T) {
	c, _, fp := callerRig(t)
	got := make(chan plugin.EventNotice, 4)
	fp.handler = func(req plugin.Request) (any, string) {
		var n plugin.EventNotice
		_ = json.Unmarshal(req.Params, &n)
		got <- n
		return map[string]any{}, ""
	}
	c.Event("probe", plugin.EventNotice{Kind: plugin.EventConfigChanged, Time: 5})
	select {
	case n := <-got:
		if n.Kind != plugin.EventConfigChanged || n.Time != 5 {
			t.Fatalf("%+v", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not delivered")
	}
	c.Stop("probe")
}

func TestCallerRejectsWrongPluginHello(t *testing.T) {
	c, _, fp := callerRig(t)
	fp.badHello = true
	if err := c.Call(context.Background(), "probe", plugin.CallRenderSlot, plugin.SlotRequest{}, nil, time.Second); err == nil {
		t.Fatal("accepted wrong hello")
	}
	fp.badHello = false
	fp.tok = "other"
	err := c.Call(context.Background(), "probe", plugin.CallRenderSlot, plugin.SlotRequest{}, nil, time.Second)
	if err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("%v", err)
	}
}

func TestCallerNotRunning(t *testing.T) {
	inv := newFakeInv()
	c := NewCaller(func() plugin.Inventory { return inv })
	if err := c.Call(context.Background(), "ghost", plugin.CallRenderSlot, nil, nil, time.Second); !errors.Is(err, plugin.ErrNotRunning) {
		t.Fatalf("%v", err)
	}
	inv.setEndpoint("ghost", filepath.Join(sockDir(t), "none.sock"), "t")
	if err := c.Call(context.Background(), "ghost", plugin.CallRenderSlot, nil, nil, time.Second); !errors.Is(err, plugin.ErrNotRunning) {
		t.Fatalf("dead socket: %v", err)
	}
}
