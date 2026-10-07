package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"vrndis/internal/stun/server"
)

func main() {
	listen := flag.String("listen", ":3478", "STUN 监听地址")
	flag.Parse()
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
