package afdp

import "testing"

func TestSharedAFDPLimitsAndDefaults(t *testing.T) {
	if maxSessionFrame != maxAFDPWireBytes || maxDatagramFrame != maxAFDPWireBytes || maxObfuscationDatagram != maxAFDPWireBytes {
		t.Fatalf("wire limit aliases drifted: base=%d session=%d datagram=%d obfuscation=%d", maxAFDPWireBytes, maxSessionFrame, maxDatagramFrame, maxObfuscationDatagram)
	}

	if defaults := DefaultQUICOptions(); defaults.MaxStreams != defaultMaxStreams {
		t.Fatalf("QUIC default streams = %d, want %d", defaults.MaxStreams, defaultMaxStreams)
	}
	if config := NewQUICConfig(QUICOptions{}); config.MaxIncomingStreams != int64(defaultMaxStreams) {
		t.Fatalf("QUIC config default streams = %d, want %d", config.MaxIncomingStreams, defaultMaxStreams)
	}

	maxFrame, maxDatagram, maxStreams, _, _, _ := (SessionOptions{
		MaxFrame:    maxSessionFrame + 1,
		MaxDatagram: maxDatagramFrame + 1,
	}).limits()
	if maxFrame != maxSessionFrame || maxDatagram != maxDatagramFrame || maxStreams != defaultMaxStreams {
		t.Fatalf("session limits = frame %d datagram %d streams %d, want %d %d %d", maxFrame, maxDatagram, maxStreams, maxSessionFrame, maxDatagramFrame, defaultMaxStreams)
	}
}
