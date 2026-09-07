package afdp

// Shared AFDP transport limits live in one place so protocol adapters cannot
// silently drift while retaining names that describe the unit each caller
// bounds (control frame, datagram, or obfuscation packet).
const (
	maxAFDPWireBytes  = 64 << 10
	maxSessionFrame   = 1 << 20
	defaultMaxStreams = 256

	maxDatagramFrame       = maxAFDPWireBytes
	maxObfuscationDatagram = maxAFDPWireBytes
)
