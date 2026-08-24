package transport

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"

	"asterferry/internal/config"
)

func BenchmarkQUICStream(b *testing.B) {
	payloadSizes := []int{16 << 10, 64 << 10}
	directions := []string{"upload", "download", "roundtrip"}
	for _, mode := range []string{config.TransportObfuscationStandard, config.TransportObfuscationCamouflage} {
		for _, direction := range directions {
			for _, payloadSize := range payloadSizes {
				for _, streams := range []int{1, 8, 32, 64} {
					mode, direction, payloadSize, streams := mode, direction, payloadSize, streams
					b.Run(fmt.Sprintf("mode=%s/direction=%s/payload=%d/streams=%d", mode, direction, payloadSize, streams), func(b *testing.B) {
						payload := make([]byte, payloadSize)
						client, cleanup := startBenchmarkQUIC(b, streams+1, mode, direction, payload)
						defer cleanup()
						connections := make([]Stream, 0, streams)
						for i := 0; i < streams; i++ {
							stream, err := client.OpenStream(context.Background())
							if err != nil {
								b.Fatal(err)
							}
							connections = append(connections, stream)
						}
						defer func() {
							for _, stream := range connections {
								_ = stream.Close()
							}
						}()
						b.SetBytes(int64(len(payload) * streams))
						b.ReportAllocs()
						b.ResetTimer()
						errs := make(chan error, streams)
						var wg sync.WaitGroup
						for _, stream := range connections {
							stream := stream
							wg.Add(1)
							go func() {
								defer wg.Done()
								result := make([]byte, len(payload))
								for i := 0; i < b.N; i++ {
									switch direction {
									case "upload":
										if err := writeAll(stream, payload); err != nil {
											errs <- err
											return
										}
									case "download":
										if _, err := io.ReadFull(stream, result); err != nil {
											errs <- err
											return
										}
									default:
										if err := writeAll(stream, payload); err != nil {
											errs <- err
											return
										}
										if _, err := io.ReadFull(stream, result); err != nil {
											errs <- err
											return
										}
									}
								}
							}()
						}
						wg.Wait()
						b.StopTimer()
						select {
						case err := <-errs:
							b.Fatal(err)
						default:
						}
					})
				}
			}
		}
	}
}

func startBenchmarkQUIC(t testing.TB, streams int, mode, direction string, payload []byte) (Session, func()) {
	t.Helper()
	serverTLS, clientTLS := benchmarkTLSConfigs(t)
	transportConfig := config.TransportConfig{
		ALPN:                           "af-raw-benchmark",
		MaxBidiRemoteStreams:           128,
		InitialStreamReceiveWindow:     1 << 20,
		InitialConnectionReceiveWindow: 8 << 20,
		MaxStreamReadBuffer:            16 << 20,
		MaxConnReadBuffer:              64 << 20,
		UDPReadBufferBytes:             16 << 20,
		UDPWriteBufferBytes:            16 << 20,
		HandshakeTimeoutSec:            10,
		IdleTimeoutSec:                 30,
	}
	obfuscation := config.TransportObfuscationOptions{Mode: mode, HandshakeShaping: mode == config.TransportObfuscationCamouflage, MinFragmentBytes: 512, MaxFragmentBytes: 1200, MaxWirePacketBytes: 1280}
	if mode == config.TransportObfuscationCamouflage {
		obfuscation.CurrentKey = []byte("raw-quic-benchmark-key-012345678901234567890123456789")
	}
	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := configureUDPBuffers(serverUDP, transportConfig); err != nil {
		_ = serverUDP.Close()
		t.Fatal(err)
	}
	serverPacketConn, err := NewObfuscatingPacketConn(serverUDP, obfuscation)
	if err != nil {
		_ = serverUDP.Close()
		t.Fatal(err)
	}
	serverTransport := &quic.Transport{Conn: serverPacketConn}
	listener, err := serverTransport.Listen(serverTLS, quicConfigWithObfuscation(transportConfig, obfuscation))
	if err != nil {
		_ = serverPacketConn.Close()
		t.Fatal(err)
	}
	serverConnCh := make(chan *quic.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept(context.Background())
		if acceptErr != nil {
			serverConnCh <- nil
			return
		}
		serverConnCh <- conn
		for {
			stream, streamErr := conn.AcceptStream(context.Background())
			if streamErr != nil {
				return
			}
			go func() {
				defer stream.Close()
				switch direction {
				case "upload":
					_, _ = io.Copy(io.Discard, stream)
				case "download":
					for {
						if err := writeAll(stream, payload); err != nil {
							return
						}
					}
				default:
					_, _ = io.Copy(stream, stream)
				}
			}()
		}
	}()
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_ = listener.Close()
		_ = serverTransport.Close()
		_ = serverPacketConn.Close()
		t.Fatal(err)
	}
	if err := configureUDPBuffers(clientUDP, transportConfig); err != nil {
		_ = clientUDP.Close()
		_ = listener.Close()
		_ = serverTransport.Close()
		_ = serverPacketConn.Close()
		t.Fatal(err)
	}
	clientPacketConn, err := NewObfuscatingPacketConn(clientUDP, obfuscation)
	if err != nil {
		_ = listener.Close()
		_ = serverTransport.Close()
		_ = serverPacketConn.Close()
		_ = clientUDP.Close()
		t.Fatal(err)
	}
	clientTransport := &quic.Transport{Conn: clientPacketConn}
	remote := listener.Addr().(*net.UDPAddr)
	clientConn, err := clientTransport.Dial(context.Background(), remote, clientTLS, quicConfigWithObfuscation(transportConfig, obfuscation))
	if err != nil {
		_ = clientTransport.Close()
		_ = clientPacketConn.Close()
		_ = listener.Close()
		_ = serverTransport.Close()
		_ = serverPacketConn.Close()
		t.Fatal(err)
	}
	serverConn := <-serverConnCh
	if serverConn == nil {
		_ = clientConn.CloseWithError(0, "server accept failed")
		_ = clientTransport.Close()
		_ = clientPacketConn.Close()
		_ = listener.Close()
		_ = serverTransport.Close()
		_ = serverPacketConn.Close()
		t.Fatal("raw QUIC server failed to accept")
	}
	client := &quicSession{conn: clientConn, closeFn: func() error { return closeTransport(clientTransport, clientPacketConn) }}
	cleanup := func() {
		_ = client.Close()
		_ = serverConn.CloseWithError(0, "benchmark complete")
		_ = listener.Close()
		_ = closeTransport(serverTransport, serverPacketConn)
	}
	return client, cleanup
}

func benchmarkTLSConfigs(t testing.TB) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "AsterFerry benchmark"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	server := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, NextProtos: []string{"af-raw-benchmark"}}
	client := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, NextProtos: []string{"af-raw-benchmark"}}
	return server, client
}
