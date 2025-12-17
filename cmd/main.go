package main

import (
	"fmt"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func main() {
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Healthy")
	})

	fmt.Println("Server started on port 8080")
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 8080})
	if err != nil {
		fmt.Println("Error starting server:", err)
	}

	go func() {
		for {
			buf := make([]byte, 1024)
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				fmt.Println("Error reading from UDP:", err)
				continue
			}
			fmt.Printf("Received %d bytes: %s\n", n, buf[:n])
		}
	}()

	// we run tcp server, as well because udp is non-blocking and it will kill the server as soon as it started
	go func() {
		err := http.ListenAndServe(":8080", r)
		if err != nil {
			fmt.Println("Error starting server:", err)
		}
	}()

	// wait for all goroutines to finish
	select {}
}
