package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"vrndis/internal/stun/server"
)

var Version = "1.1.0+unknown"

func main() {
	listen := flag.String("listen", ":5478", "STUN 监听地址")
	showVersion := flag.Bool("version", false, "显示版本")
	flag.Parse()

	if *showVersion {
		fmt.Printf("stun-server %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return
	}

	srv := server.New(server.Config{ListenAddr: *listen})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		if err := srv.Start(); err != nil {
			log.Fatalf("STUN server error: %v", err)
		}
	}()
	<-sigCh
	log.Println("shutting down STUN server")
	srv.Stop()
}
