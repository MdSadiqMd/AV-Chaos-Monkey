package main

import (
	"fmt"
	"net"
)

func main() {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:8080")
	if err != nil {
		fmt.Println(err)
		return
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer conn.Close()

	fmt.Println("Connected to server")
	fmt.Println("Sending message...")

	message := []byte("Hello, server!")
	_, err = conn.Write(message)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("Message sent")
}
