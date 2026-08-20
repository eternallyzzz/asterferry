package transport

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	Version         byte = 2
	HeaderSize           = 16 // version, type, reserved, request ID
	DefaultMaxFrame      = 16 << 20

	TypeHello byte = iota + 1
	TypeChallenge
	TypeAuth
	TypeAuthOK
	TypeRegister
	TypeRegisterResult
	TypeOpenProxy
	TypeOpenReverse
	TypeOpenOK
	TypeOpenError
	TypeData
	TypePing
	TypePong
)

type Frame struct {
	Version   byte
	Type      byte
	RequestID uint64
	Payload   []byte
}

func WriteFrame(w io.Writer, f Frame, max int64) error {
	if f.Version == 0 {
		f.Version = Version
	}
	if f.Version != Version {
		return fmt.Errorf("unsupported protocol version %d", f.Version)
	}
	if !validType(f.Type) {
		return fmt.Errorf("unsupported frame type %d", f.Type)
	}
	if max <= 0 {
		max = DefaultMaxFrame
	}
	if int64(len(f.Payload))+HeaderSize > max {
		return errors.New("frame exceeds configured limit")
	}
	length := uint32(HeaderSize + len(f.Payload))
	header := make([]byte, 4+HeaderSize)
	binary.BigEndian.PutUint32(header[:4], length)
	header[4] = f.Version
	header[5] = f.Type
	binary.BigEndian.PutUint64(header[12:20], f.RequestID)
	if err := writeAll(w, header); err != nil {
		return err
	}
	if len(f.Payload) == 0 {
		return nil
	}
	return writeAll(w, f.Payload)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
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

func ReadFrame(r io.Reader, max int64) (Frame, error) {
	if max <= 0 {
		max = DefaultMaxFrame
	}
	var lengthBytes [4]byte
	if _, err := io.ReadFull(r, lengthBytes[:]); err != nil {
		return Frame{}, err
	}
	length := int64(binary.BigEndian.Uint32(lengthBytes[:]))
	if length < HeaderSize || length > max {
		return Frame{}, fmt.Errorf("invalid frame length %d", length)
	}
	b := make([]byte, length)
	if _, err := io.ReadFull(r, b); err != nil {
		return Frame{}, err
	}
	if b[0] != Version {
		return Frame{}, fmt.Errorf("unsupported protocol version %d", b[0])
	}
	if !validType(b[1]) {
		return Frame{}, fmt.Errorf("unsupported frame type %d", b[1])
	}
	for _, value := range b[2:8] {
		if value != 0 {
			return Frame{}, errors.New("non-zero frame reserved bits")
		}
	}
	f := Frame{Version: b[0], Type: b[1], RequestID: binary.BigEndian.Uint64(b[8:16])}
	f.Payload = append([]byte(nil), b[HeaderSize:]...)
	return f, nil
}

func JSONFrame(typ byte, requestID uint64, v any) (Frame, error) {
	if !validType(typ) {
		return Frame{}, fmt.Errorf("unsupported frame type %d", typ)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return Frame{}, err
	}
	return Frame{Version: Version, Type: typ, RequestID: requestID, Payload: b}, nil
}

func validType(typ byte) bool {
	return typ >= TypeHello && typ <= TypePong
}

func DecodeJSON(f Frame, v any) error {
	if len(f.Payload) == 0 {
		return errors.New("empty JSON payload")
	}
	return json.Unmarshal(f.Payload, v)
}

type Hello struct {
	AgentID string `json:"agent_id"`
}
type Challenge struct {
	Nonce []byte `json:"nonce"`
}
type Auth struct {
	MAC []byte `json:"mac"`
}
type AuthResult struct {
	Error string `json:"error,omitempty"`
}

type TunnelRegistration struct {
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	GatewayPort uint16 `json:"gateway_port"`
	Profile     string `json:"profile"`
}

type Register struct {
	Mappings []TunnelRegistration `json:"mappings"`
}
type RegisterResult struct {
	Error    string               `json:"error,omitempty"`
	Mappings []TunnelRegistration `json:"mappings,omitempty"`
}

type OpenProxy struct {
	Network string `json:"network"`
	Address string `json:"address"`
	Port    uint16 `json:"port"`
	Profile string `json:"profile"`
}

type OpenReverse struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Profile  string `json:"profile"`
}

type OpenResult struct {
	Error string `json:"error,omitempty"`
}

type Data struct {
	Payload []byte `json:"payload"`
	Padding []byte `json:"padding,omitempty"`
}

func DecodeData(f Frame, maxPayload, maxPadding int64) (Data, error) {
	var data Data
	if err := DecodeJSON(f, &data); err != nil {
		return Data{}, err
	}
	if maxPayload < 1 || int64(len(data.Payload)) > maxPayload || int64(len(data.Padding)) > maxPadding {
		return Data{}, errors.New("data record exceeds configured limit")
	}
	return data, nil
}

func NewData(payload []byte, profile string, maxPadding int64) Data {
	data := Data{Payload: append([]byte(nil), payload...)}
	if profile != "balanced" || maxPadding <= 0 {
		return data
	}
	base := int64(len(payload) + 64) // JSON/base64 overhead is intentionally conservative.
	for _, bucket := range []int64{512, 1024, 2048, 4096, 8192, 16384} {
		if bucket < base || bucket-base > maxPadding {
			continue
		}
		available := bucket - base
		var b [2]byte
		if _, err := rand.Read(b[:]); err != nil {
			return data
		}
		paddingLen := int(binary.BigEndian.Uint16(b[:])) % int(available+1)
		if paddingLen > 0 {
			data.Padding = make([]byte, paddingLen)
			_, _ = rand.Read(data.Padding)
		}
		return data
	}
	return data
}

func NewNonce() ([]byte, error) {
	n := make([]byte, 32)
	_, err := rand.Read(n)
	return n, err
}

func SignChallenge(token, nonce []byte, agentID string) []byte {
	h := hmac.New(sha256.New, token)
	h.Write([]byte("asterferry/v2/auth/"))
	h.Write([]byte(agentID))
	h.Write(nonce)
	return h.Sum(nil)
}

func VerifyChallenge(token, nonce, mac []byte, agentID string) bool {
	expected := SignChallenge(token, nonce, agentID)
	return hmac.Equal(expected, mac)
}
