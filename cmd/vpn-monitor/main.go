package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/akorzunin/vpn-monitor/internal/monitor"
)

func main() {
	config := flag.String("config", "config.yaml", "YAML configuration path")
	listen := flag.String("listen", "", "override the configured HTTP listen address")
	echo := flag.Bool("echo", false, "run only the authenticated UDP echo endpoint")
	echoListen := flag.String("echo-listen", "127.0.0.1:9444", "UDP echo listen address")
	echoTokenEnv := flag.String("echo-token-env", "VPN_MONITOR_UDP_TOKEN", "UDP echo secret environment variable")
	check := flag.Bool("check", false, "validate configuration without running tests")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *echo {
		if err := monitor.Echo(ctx, *echoListen, os.Getenv(*echoTokenEnv)); err != nil {
			log.Fatal(err)
		}
		return
	}
	cfg, err := monitor.LoadConfig(*config)
	if err != nil {
		log.Fatal(err)
	}
	if *listen != "" {
		if _, _, err := net.SplitHostPort(*listen); err != nil {
			log.Fatal("listen override must be host:port")
		}
		cfg.Listen = *listen
	}
	if *check {
		log.Printf("configuration valid: %d routes", len(cfg.Routes))
		return
	}
	if monitor.VerifyWeb(cfg.WebDir) != nil {
		log.Fatal("web build not found; run npm ci && npm run build in web/")
	}
	service, err := monitor.NewService(cfg)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Fatal("cannot bind HTTP listen address")
	}
	server := &http.Server{Addr: cfg.Listen, Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	service.Start(ctx)
	go func() {
		log.Printf("listening on %s (%d routes)", cfg.Listen, len(cfg.Routes))
		if e := server.Serve(listener); e != nil && e != http.ErrServerClosed {
			log.Print("HTTP listener failed")
			cancel()
		}
	}()
	<-ctx.Done()
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	service.Wait()
}
