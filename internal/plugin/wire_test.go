package plugin

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWriteFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, map[string]string{"x": strings.Repeat("a", MaxMessageBytes+1)})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatal("oversize frame was partially written")
	}
}

func TestReadFrameRejectsOversizeLine(t *testing.T) {
	line := `{"x":"` + strings.Repeat("a", 2*MaxMessageBytes) + `"}` + "\n"
	r := bufio.NewReader(strings.NewReader(line))
	var v map[string]string
	if err := ReadFrame(r, &v); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestReadFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Request{ID: 7, Method: MethodLog, Params: []byte(`{"a":1}`), Node: "n"}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, Hello{Hello: PluginHelloMagic, Version: 1}); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	var out Request
	if err := ReadFrame(r, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != 7 || out.Method != MethodLog || string(out.Params) != `{"a":1}` || out.Node != "n" {
		t.Fatalf("got %+v", out)
	}
	var h Hello
	if err := ReadFrame(r, &h); err != nil || h.Hello != PluginHelloMagic {
		t.Fatalf("hello %+v %v", h, err)
	}
	if err := ReadFrame(r, &h); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReadFrameAcceptsLineNearCap(t *testing.T) {
	big := strings.Repeat("a", MaxMessageBytes-100)
	var buf bytes.Buffer
	if err := WriteFrame(&buf, map[string]string{"x": big}); err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := ReadFrame(bufio.NewReaderSize(&buf, 4096), &v); err != nil || len(v["x"]) != len(big) {
		t.Fatalf("got %v", err)
	}
}
