package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p := pluginsdk.New(pluginsdk.Options{Version: "1.0.0"})
	p.OnConfig(func(c pluginsdk.Config) {
		if c.String("mode") == "crash" {
			os.Exit(3)
		}
	})
	p.HandleSlot("main", func(ctx context.Context, req plugin.SlotRequest) ([]plugin.Widget, error) {
		return []plugin.Widget{
			pluginsdk.Stat("Latency", "42", "ms"),
			pluginsdk.Text("Greeting", "E2E-WIDGET-OK"),
		}, nil
	})
	go report(ctx, p)
	if err := p.Run(ctx); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

func report(ctx context.Context, p *pluginsdk.Plugin) {
	raised := false
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := p.Publish(0, map[string]float64{"latency": 42}); err != nil {
			continue
		}
		if !raised && p.RaiseAlert("probe.down", "warning", "Probe down", "e2e probe failed") == nil {
			raised = true
			_ = p.ReportStatus("ok", "running")
		}
	}
}
