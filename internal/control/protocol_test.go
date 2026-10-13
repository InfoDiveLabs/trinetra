package control

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
)

func TestFrameRoundTripRequest(t *testing.T) {
	var buf bytes.Buffer
	want := request{ID: 7, Method: "Snapshot", Params: json.RawMessage(`{"a":1}`)}
	if err := writeFrame(&buf, want); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	r := bufio.NewReader(&buf)
	var got request
	if err := readFrame(r, &got); err != nil {
		t.Fatalf("readFrame: %v", err)
	}

	if got.ID != want.ID {
		t.Errorf("ID = %d, want %d", got.ID, want.ID)
	}
	if got.Method != want.Method {
		t.Errorf("Method = %q, want %q", got.Method, want.Method)
	}
	if !bytes.Equal(got.Params, want.Params) {
		t.Errorf("Params = %s, want %s", got.Params, want.Params)
	}
}

func TestFrameRoundTripResponse(t *testing.T) {
	var buf bytes.Buffer
	want := response{ID: 3, OK: false, Result: json.RawMessage(`null`), Error: "boom"}
	if err := writeFrame(&buf, want); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	r := bufio.NewReader(&buf)
	var got response
	if err := readFrame(r, &got); err != nil {
		t.Fatalf("readFrame: %v", err)
	}

	if got.ID != want.ID || got.OK != want.OK || got.Error != want.Error {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !bytes.Equal(got.Result, want.Result) {
		t.Errorf("Result = %s, want %s", got.Result, want.Result)
	}
}

func TestFrameRoundTripHello(t *testing.T) {
	var buf bytes.Buffer
	want := hello{Hello: "serverwatch-control", Version: ProtocolVersion}
	if err := writeFrame(&buf, want); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	r := bufio.NewReader(&buf)
	var got hello
	if err := readFrame(r, &got); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestFrameWriteAppendsNewlineAndMultipleFramesReadInOrder: frames are
// newline-delimited and read back one at a time, in order.
func TestFrameWriteAppendsNewlineAndMultipleFramesReadInOrder(t *testing.T) {
	var buf bytes.Buffer
	first := request{ID: 1, Method: "Snapshot", Params: json.RawMessage(`{}`)}
	second := request{ID: 2, Method: "Monitoring", Params: json.RawMessage(`{}`)}

	if err := writeFrame(&buf, first); err != nil {
		t.Fatalf("writeFrame(first): %v", err)
	}
	if err := writeFrame(&buf, second); err != nil {
		t.Fatalf("writeFrame(second): %v", err)
	}

	if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 2 {
		t.Fatalf("expected 2 newline-delimited frames, found %d newlines", n)
	}

	r := bufio.NewReader(&buf)
	var gotFirst, gotSecond request
	if err := readFrame(r, &gotFirst); err != nil {
		t.Fatalf("readFrame(first): %v", err)
	}
	if err := readFrame(r, &gotSecond); err != nil {
		t.Fatalf("readFrame(second): %v", err)
	}

	if gotFirst.ID != 1 || gotFirst.Method != "Snapshot" {
		t.Errorf("first = %+v, want ID=1 Method=Snapshot", gotFirst)
	}
	if gotSecond.ID != 2 || gotSecond.Method != "Monitoring" {
		t.Errorf("second = %+v, want ID=2 Method=Monitoring", gotSecond)
	}
}
