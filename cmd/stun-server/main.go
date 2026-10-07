// Command stun-server 提供独立的 STUN 服务器
// 用法: stun-server [--listen :3478]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"vrndis/internal/stun/server"
)

func main() {
	log.SetFlags(log.LstdFlags)
	listen := flag.String("listen", ":3478", "STUN 监听地址")
	flag.Parse()

	srv := server.New(server.Config{
		ListenAddr:   *listen,
		RealmName:    "vrndis",
		OverwriteNAT: false,
	})

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
	fmt.Println("\nSTUN server stopped")
}
