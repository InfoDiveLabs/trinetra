package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
)

const version = "0.1.0"

func settingsFrom(c pluginsdk.Config) settings {
	return settings{listen: c.String("listen"), token: c.String("bearer_token"), fleet: c.Bool("include_fleet")}
}

func validateListen(c pluginsdk.Config) pluginsdk.FieldErrors {
	_, port, err := net.SplitHostPort(c.String("listen"))
	if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 0 || n > 65535 {
		return pluginsdk.FieldErrors{"listen": "must be host:port, for example 127.0.0.1:9469"}
	}
	return nil
}

func newPlugin() (*pluginsdk.Plugin, *exporter) {
	p := pluginsdk.New(pluginsdk.Options{Version: version})
	exp := newExporter(p, func(state, msg string) { _ = p.ReportStatus(state, msg) })
	p.HandleValidateConfig(validateListen)
	p.OnConfig(func(c pluginsdk.Config) { exp.apply(settingsFrom(c)) })
	return p, exp
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p, exp := newPlugin()
	defer exp.close()
	if err := p.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
