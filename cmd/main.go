package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/server"
)

func main() {
	httpServer := server.NewHTTPServer(":8080")

	errChan := make(chan error, 1)
	go func() {
		if err := httpServer.Start(); err != nil {
			errChan <- err
		}
	}()

	log.Printf("Server started on :8080")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigChan:
		log.Printf("[Main] Received signal %v, shutting down...", sig)
	case err := <-errChan:
		log.Printf("[Main] Server error: %v", err)
	}

	log.Println("[Main] Shutdown complete")
}
