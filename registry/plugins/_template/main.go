package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
)

const version = "0.1.0"

func newPlugin() *pluginsdk.Plugin {
	p := pluginsdk.New(pluginsdk.Options{Version: version})
	p.HandleSlot("hello", func(ctx context.Context, req pluginsdk.SlotRequest) ([]pluginsdk.Widget, error) {
		return []pluginsdk.Widget{pluginsdk.Text("Hello", "Hello from the template plugin.")}, nil
	})
	return p
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newPlugin().Run(ctx); err != nil {
		log.Fatal(err)
	}
}
