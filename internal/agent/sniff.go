package agent

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"time"
)

type sniffResult struct {
	Protocol string
	Domain   string
}

// sniffTLS reads only a bounded prefix and returns a reader that replays every
// consumed byte. A failed or incomplete ClientHello is never a proxy error.
func sniffTLS(conn net.Conn, reader io.Reader, maxBytes int64, timeout time.Duration) (sniffResult, io.Reader) {
	if conn == nil || reader == nil || maxBytes < 5 {
		return sniffResult{}, reader
	}
	if maxBytes > 1<<20 {
		maxBytes = 1 << 20
	}
	previous := time.Time{}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = conn.SetReadDeadline(previous) }()
	prefix := make([]byte, 0, int(maxBytes))
	buf := make([]byte, minInt64(maxBytes, 4096))
	for int64(len(prefix)) < maxBytes {
		want := buf
		remaining := int(maxBytes) - len(prefix)
		if remaining < len(want) {
			want = want[:remaining]
		}
		n, err := reader.Read(want)
		if n > 0 {
			prefix = append(prefix, want[:n]...)
			if len(prefix) >= 5 {
				if prefix[0] != 22 || prefix[1] != 3 {
					break
				}
				if result := parseTLSClientHello(prefix); result.Domain != "" {
					break
				}
			}
		}
		if err != nil {
			break
		}
		if n == 0 {
			break
		}
	}
	result := parseTLSClientHello(prefix)
	return result, io.MultiReader(bytes.NewReader(prefix), reader)
}

func parseTLSClientHello(record []byte) sniffResult {
	var handshake []byte
	for pos := 0; pos+5 <= len(record); {
		if record[pos] != 22 || record[pos+1] != 3 {
			return sniffResult{}
		}
		recordLength := int(binary.BigEndian.Uint16(record[pos+3 : pos+5]))
		if recordLength < 1 || pos+5+recordLength > len(record) {
			break
		}
		handshake = append(handshake, record[pos+5:pos+5+recordLength]...)
		pos += 5 + recordLength
		if len(handshake) < 4 || handshake[0] != 1 {
			continue
		}
		handshakeLength := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
		if handshakeLength < 34 || len(handshake) < 4+handshakeLength {
			continue
		}
		return parseClientHelloBody(handshake[4 : 4+handshakeLength])
	}
	return sniffResult{}
}

func parseClientHelloBody(b []byte) sniffResult {
	if len(b) < 34 {
		return sniffResult{}
	}
	pos := 2 + 32
	if pos >= len(b) {
		return sniffResult{}
	}
	sessionLen := int(b[pos])
	pos++
	if !take(&pos, sessionLen, len(b)) {
		return sniffResult{}
	}
	if !takeVector(b, &pos, 2, len(b)) || !takeVector(b, &pos, 1, len(b)) || pos+2 > len(b) {
		return sniffResult{}
	}
	extLen := int(binary.BigEndian.Uint16(b[pos : pos+2]))
	pos += 2
	if pos+extLen > len(b) {
		return sniffResult{}
	}
	end := pos + extLen
	for pos+4 <= end {
		extType := binary.BigEndian.Uint16(b[pos : pos+2])
		length := int(binary.BigEndian.Uint16(b[pos+2 : pos+4]))
		pos += 4
		if pos+length > end {
			return sniffResult{}
		}
		if extType == 0 && length >= 5 {
			nameListLen := int(binary.BigEndian.Uint16(b[pos : pos+2]))
			if nameListLen+2 > length || pos+4 > end {
				return sniffResult{}
			}
			namePos := pos + 2
			nameEnd := namePos + nameListLen
			for namePos+3 <= nameEnd {
				nameType := b[namePos]
				nameLen := int(binary.BigEndian.Uint16(b[namePos+1 : namePos+3]))
				namePos += 3
				if namePos+nameLen > nameEnd {
					return sniffResult{}
				}
				if nameType == 0 {
					domain := normalizeSniffDomain(string(b[namePos : namePos+nameLen]))
					if domain != "" {
						return sniffResult{Protocol: "tls_sni", Domain: domain}
					}
				}
				namePos += nameLen
			}
		}
		pos += length
	}
	return sniffResult{}
}

func take(pos *int, n, limit int) bool {
	if n < 0 || *pos+n > limit {
		return false
	}
	*pos += n
	return true
}

func takeVector(b []byte, pos *int, width, limit int) bool {
	if *pos+width > limit {
		return false
	}
	var n int
	switch width {
	case 1:
		n = int(b[*pos])
	case 2:
		n = int(binary.BigEndian.Uint16(b[*pos : *pos+2]))
	default:
		return false
	}
	*pos += width
	return take(pos, n, limit)
}

func normalizeSniffDomain(value string) string {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if value == "" || len(value) > 253 || strings.ContainsAny(value, "\x00\r\n /\\") {
		return ""
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return ""
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return ""
			}
		}
	}
	return value
}

func minInt64(a, b int64) int {
	if a < b {
		return int(a)
	}
	return int(b)
}
