package duplex

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"testing"
	"time"

	"asterferry/internal/afdp"

	"github.com/quic-go/quic-go"
)

type loopbackQUICPair struct {
	listener *quic.Listener
	client   *quic.Conn
	server   *quic.Conn
}

func newLoopbackQUICPair(t *testing.T) *loopbackQUICPair {
	t.Helper()
	certificate := newLoopbackCertificate(t)
	serverTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, NextProtos: []string{afdp.ALPN}}
	clientTLS := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{afdp.ALPN}} // #nosec G402 -- this is an in-memory loopback test certificate.
	options := afdp.DefaultQUICOptions()
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, afdp.NewQUICConfig(options))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan *quic.Conn, 1)
	acceptErr := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	go func() {
		connection, acceptError := listener.Accept(ctx)
		if acceptError != nil {
			acceptErr <- acceptError
			return
		}
		accepted <- connection
	}()
	client, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, afdp.NewQUICConfig(options))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CloseWithError(0, "test complete") })
	var server *quic.Conn
	select {
	case server = <-accepted:
	case err := <-acceptErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Cleanup(func() { _ = server.CloseWithError(0, "test complete") })
	return &loopbackQUICPair{listener: listener, client: client, server: server}
}

func newLoopbackCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "asterferry-loopback"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestQUICStreamHalfCloseContract(t *testing.T) {
	pair := newLoopbackQUICPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := make(chan struct {
		stream *quic.Stream
		err    error
	}, 1)
	go func() {
		stream, acceptErr := pair.server.AcceptStream(ctx)
		accepted <- struct {
			stream *quic.Stream
			err    error
		}{stream: stream, err: acceptErr}
	}()
	clientStream, err := pair.client.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientStream.Write([]byte("request-after-fin")); err != nil {
		t.Fatal(err)
	}
	if err := clientStream.Close(); err != nil {
		t.Fatal(err)
	}
	var acceptedStream struct {
		stream *quic.Stream
		err    error
	}
	select {
	case acceptedStream = <-accepted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if acceptedStream.err != nil {
		t.Fatal(acceptedStream.err)
	}
	serverStream := acceptedStream.stream
	serverResult := make(chan error, 1)
	go func() {
		request, readErr := io.ReadAll(serverStream)
		if readErr != nil {
			serverResult <- readErr
			return
		}
		if string(request) != "request-after-fin" {
			serverResult <- &streamContractError{message: "server received unexpected request"}
			return
		}
		if _, writeErr := serverStream.Write([]byte("response-after-fin")); writeErr != nil {
			serverResult <- writeErr
			return
		}
		serverResult <- serverStream.Close()
	}()
	response, err := io.ReadAll(clientStream)
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != "response-after-fin" {
		t.Fatalf("response after client FIN = %q", response)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

type streamContractError struct {
	message string
}

func (e *streamContractError) Error() string { return e.message }

func TestQUICDatagramReassemblyContract(t *testing.T) {
	pair := newLoopbackQUICPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	payload := []byte("datagrams preserve ordering only after the reassembler completes the flow")
	frames, err := afdp.Fragments(42, 7, payload, 64)
	if err != nil {
		t.Fatal(err)
	}
	reassembler, err := afdp.NewReassembler(8, 4096, 128, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for index := len(frames) - 1; index >= 0; index-- {
		if err := pair.client.SendDatagram(frames[index]); err != nil {
			t.Fatal(err)
		}
	}
	var result []byte
	for result == nil {
		frame, err := pair.server.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatal(err)
		}
		value, complete, err := reassembler.Add(frame, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			result = value
		}
	}
	if string(result) != string(payload) {
		t.Fatalf("reassembled datagram payload = %q, want %q", result, payload)
	}
}
