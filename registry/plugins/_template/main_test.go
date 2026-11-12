package main

import (
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk/pluginsdktest"
)

func TestHelloSlotRendersGreeting(t *testing.T) {
	h := pluginsdktest.New(t, "hello", nil, nil)
	h.Start(newPlugin())
	res, err := h.Render("hello", plugin.SlotContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Widgets) != 1 {
		t.Fatalf("widgets = %d, want 1", len(res.Widgets))
	}
}
