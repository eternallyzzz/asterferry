package dataplane

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"
)

func TestWriteReverseFlowPacketUsesFlowAddress(t *testing.T) {
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	flowID := uint64(7)
	flows := map[string]*udpReverseFlow{
		"client": {id: flowID, addr: receiver.LocalAddr().(*net.UDPAddr)},
	}
	var flowsMu sync.Mutex
	payload := []byte("reverse payload")
	if err := writeReverseFlowPacket(sender, &flowsMu, flows, flowID, payload); err != nil {
		t.Fatal(err)
	}

	_ = receiver.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, len(payload)+1)
	n, _, err := receiver.ReadFromUDP(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:n], payload) {
		t.Fatalf("payload mismatch: got %q", got[:n])
	}
}

func TestWriteReverseFlowPacketSkipsMissingFlow(t *testing.T) {
	sender, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	var flowsMu sync.Mutex
	if err := writeReverseFlowPacket(sender, &flowsMu, map[string]*udpReverseFlow{}, 42, []byte("ignored")); err != nil {
		t.Fatal(err)
	}
}
