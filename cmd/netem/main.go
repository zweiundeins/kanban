// Command netem is a TCP proxy that behaves like a slow network: every
// chunk waits half the round trip in each direction, and each direction is
// limited in bandwidth. Unlike a browser's network emulation, it delays
// what arrives on an open stream or WebSocket too, so both apps of a
// comparison pay the same round trips.
//
//	go run ./cmd/netem -listen 127.0.0.1:9000 -to 127.0.0.1:8080 -rtt 150ms -down 1638 -up 750
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"time"
)

var (
	listen = flag.String("listen", "127.0.0.1:9000", "address to listen on")
	to     = flag.String("to", "127.0.0.1:8080", "the server")
	rtt    = flag.Duration("rtt", 150*time.Millisecond, "round-trip time added")
	down   = flag.Int("down", 1638, "server to client, kbit/s (0: unlimited)")
	up     = flag.Int("up", 750, "client to server, kbit/s (0: unlimited)")
)

type chunk struct {
	data []byte
	at   time.Time // when it may be delivered
}

// pipe copies src to dst, delivering each chunk half an RTT after it was
// read, no faster than kbps.
func pipe(dst, src net.Conn, kbps int) {
	ch := make(chan chunk, 1024)
	go func() {
		defer close(ch)
		for {
			buf := make([]byte, 32<<10)
			n, err := src.Read(buf)
			if n > 0 {
				ch <- chunk{buf[:n], time.Now().Add(*rtt / 2)}
			}
			if err != nil {
				return
			}
		}
	}()
	var free time.Time // when the link is free again
	for c := range ch {
		if d := time.Until(c.at); d > 0 {
			time.Sleep(d)
		}
		if kbps > 0 {
			if now := time.Now(); free.Before(now) {
				free = now
			}
			free = free.Add(time.Duration(len(c.data)) * 8 * time.Second / time.Duration(kbps*1000))
			if d := time.Until(free); d > 0 {
				time.Sleep(d)
			}
		}
		if _, err := dst.Write(c.data); err != nil {
			break
		}
	}
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	io.Copy(io.Discard, src)
}

func main() {
	flag.Parse()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%s -> %s, rtt %s, down %d kbit/s, up %d kbit/s", *listen, *to, *rtt, *down, *up)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			s, err := net.Dial("tcp", *to)
			if err != nil {
				c.Close()
				return
			}
			go pipe(s, c, *up)
			pipe(c, s, *down)
			c.Close()
			s.Close()
		}()
	}
}
