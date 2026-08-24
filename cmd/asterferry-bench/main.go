// Command asterferry-bench contains small, dependency-free helpers used by
// the cross-platform performance scripts. It is deliberately separate from
// the production binary: the helper never handles credentials or implements a
// second AsterFerry protocol.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type result struct {
	Direction string  `json:"direction"`
	Target    string  `json:"target,omitempty"`
	Streams   int     `json:"streams"`
	Payload   int     `json:"payload_bytes"`
	Seconds   float64 `json:"seconds"`
	Bytes     uint64  `json:"bytes"`
	Errors    uint64  `json:"errors"`
	Mbps      float64 `json:"mbps"`
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: asterferry-bench <echo|load> [flags]")
	}
	var err error
	switch os.Args[1] {
	case "echo":
		err = runEcho(os.Args[2:])
	case "load":
		err = runLoad(os.Args[2:])
	default:
		err = fmt.Errorf("unknown benchmark command %q", os.Args[1])
	}
	if err != nil {
		fatal(err.Error())
	}
}

func runEcho(args []string) error {
	fs := flag.NewFlagSet("echo", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:39090", "TCP listen address")
	mode := fs.String("mode", "echo", "echo, discard, or source")
	payloadBytes := fs.Int("payload", 64<<10, "source payload size")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mode != "echo" && *mode != "discard" && *mode != "source" {
		return errors.New("echo mode must be echo, discard, or source")
	}
	if *payloadBytes < 1 || *payloadBytes > 16<<20 {
		return errors.New("echo payload must be between 1 byte and 16 MiB")
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	payload := make([]byte, *payloadBytes)
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			return acceptErr
		}
		go serveEcho(ctx, conn, *mode, payload)
	}
}

func serveEcho(ctx context.Context, conn net.Conn, mode string, payload []byte) {
	defer conn.Close()
	switch mode {
	case "discard":
		_, _ = io.Copy(io.Discard, conn)
	case "source":
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err := writeAll(conn, payload); err != nil {
				return
			}
		}
	default:
		_, _ = io.Copy(conn, conn)
	}
}

func runLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	target := fs.String("target", "127.0.0.1:39090", "TCP target address")
	direction := fs.String("direction", "roundtrip", "upload, download, or roundtrip")
	streams := fs.Int("streams", 8, "parallel TCP streams")
	payloadBytes := fs.Int("payload", 64<<10, "payload size")
	seconds := fs.Duration("duration", 30*time.Second, "measurement duration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *direction != "upload" && *direction != "download" && *direction != "roundtrip" {
		return errors.New("load direction must be upload, download, or roundtrip")
	}
	if *streams < 1 || *streams > 4096 {
		return errors.New("streams must be between 1 and 4096")
	}
	if *payloadBytes < 1 || *payloadBytes > 16<<20 {
		return errors.New("payload must be between 1 byte and 16 MiB")
	}
	if *seconds <= 0 || *seconds > time.Hour {
		return errors.New("duration must be between 1ns and 1h")
	}
	payload := make([]byte, *payloadBytes)
	var total atomic.Uint64
	var failures atomic.Uint64
	var wg sync.WaitGroup
	startGate := make(chan struct{})
	var start time.Time
	var deadline time.Time
	for i := 0; i < *streams; i++ {
		conn, err := net.DialTimeout("tcp", *target, 10*time.Second)
		if err != nil {
			failures.Add(1)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			<-startGate
			_ = conn.SetDeadline(deadline)
			buffer := make([]byte, len(payload))
			for time.Now().Before(deadline) {
				switch *direction {
				case "upload":
					if err := writeAll(conn, payload); err != nil {
						if time.Now().Before(deadline) {
							failures.Add(1)
						}
						return
					}
					total.Add(uint64(len(payload)))
				case "download":
					n, err := io.ReadFull(conn, buffer)
					if n > 0 {
						total.Add(uint64(n))
					}
					if err != nil {
						if time.Now().Before(deadline) {
							failures.Add(1)
						}
						return
					}
				default:
					if err := writeAll(conn, payload); err != nil {
						if time.Now().Before(deadline) {
							failures.Add(1)
						}
						return
					}
					if _, err := io.ReadFull(conn, buffer); err != nil {
						if time.Now().Before(deadline) {
							failures.Add(1)
						}
						return
					}
					total.Add(uint64(len(payload)))
				}
			}
		}()
	}
	start = time.Now()
	deadline = start.Add(*seconds)
	close(startGate)
	wg.Wait()
	actual := time.Since(start)
	if actual <= 0 {
		actual = *seconds
	}
	mbps := float64(total.Load()*8) / actual.Seconds() / 1e6
	if err := json.NewEncoder(os.Stdout).Encode(result{
		Direction: *direction,
		Target:    *target,
		Streams:   *streams,
		Payload:   *payloadBytes,
		Seconds:   actual.Seconds(),
		Bytes:     total.Load(),
		Errors:    failures.Load(),
		Mbps:      mbps,
	}); err != nil {
		return err
	}
	if total.Load() == 0 {
		return errors.New("load produced no payload")
	}
	if failures.Load() > 0 {
		return fmt.Errorf("load encountered %d stream errors", failures.Load())
	}
	return nil
}

func writeAll(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n > 0 {
			payload = payload[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func fatal(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(2)
}
