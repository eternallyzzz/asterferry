package transport

import (
	"reflect"
	"strings"
	"testing"
)

func TestMessageVariantsRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		typ    byte
		value  any
		target any
	}{
		{"hello", TypeHello, Hello{AgentID: "edge", Capabilities: []Capability{CapabilityErrorsV1, CapabilityLimitsV1}, Limits: Limits{MaxFrameBytes: 4096}}, &Hello{}},
		{"challenge", TypeChallenge, Challenge{Nonce: []byte("nonce"), Capabilities: []Capability{CapabilityErrorsV1, CapabilityLimitsV1}, Limits: Limits{MaxStreams: 4}}, &Challenge{}},
		{"auth", TypeAuth, Auth{MAC: []byte("mac")}, &Auth{}},
		{"auth-ok", TypeAuthOK, AuthResult{Error: NewProtocolError(ErrorAuthFailed, "denied", false)}, &AuthResult{}},
		{"register", TypeRegister, Register{Mappings: []TunnelRegistration{{Name: "web", Protocol: "tcp", GatewayPort: 8080, Profile: "standard"}}}, &Register{}},
		{"register-result", TypeRegisterResult, RegisterResult{Mappings: []TunnelRegistration{{Name: "dns", Protocol: "udp", GatewayPort: 5353, Profile: "balanced"}}, Error: NewProtocolError(ErrorMappingRejected, "busy", true)}, &RegisterResult{}},
		{"open-proxy", TypeOpenProxy, OpenProxy{Network: "tcp", Address: "example.com", Port: 443, Profile: "standard"}, &OpenProxy{}},
		{"open-reverse", TypeOpenReverse, OpenReverse{Name: "web", Protocol: "tcp", Profile: "balanced"}, &OpenReverse{}},
		{"open-result", TypeOpenOK, OpenResult{}, &OpenResult{}},
		{"open-error", TypeOpenError, OpenResult{Error: NewProtocolError(ErrorPolicyDenied, "denied", false)}, &OpenResult{}},
		{"data", TypeData, Data{Payload: []byte("payload"), Padding: []byte("pad")}, &Data{}},
		{"error", TypeError, *NewProtocolError(ErrorInternal, "internal", true), &ProtocolError{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := MessageFrame(tc.typ, 7, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if err := DecodeMessage(frame, tc.target); err != nil {
				t.Fatal(err)
			}
			if tc.typ == TypeData {
				data := tc.target.(*Data)
				if !reflect.DeepEqual(data.Payload, []byte("payload")) || !reflect.DeepEqual(data.Padding, []byte("pad")) {
					t.Fatalf("data round trip = %#v", data)
				}
			}
		})
	}
	canonical, err := MessageFrame(TypeHello, 0, Hello{Capabilities: []Capability{CapabilityReverseTCP, CapabilityErrorsV1, CapabilityLimitsV1}})
	if err != nil {
		t.Fatal(err)
	}
	var decodedHello Hello
	if err := DecodeMessage(canonical, &decodedHello); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decodedHello.Capabilities, []Capability{CapabilityErrorsV1, CapabilityLimitsV1, CapabilityReverseTCP}) {
		t.Fatalf("capabilities were not canonicalized: %#v", decodedHello.Capabilities)
	}

	if _, err := MessageFrame(TypePing, 0, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := MessageFrame(TypePing, 0, "not-empty"); err == nil {
		t.Fatal("empty message accepted wrong payload")
	}
	if _, err := MessageFrame(0xff, 0, nil); err == nil {
		t.Fatal("unknown frame type accepted")
	}
	if err := DecodeMessage(Frame{Version: Version, Type: TypeData, Payload: nil}, nil); err == nil {
		t.Fatal("missing decode target accepted")
	}
	if err := DecodeMessage(Frame{Version: 3, Type: TypePing}, nil); err == nil {
		t.Fatal("wrong frame version accepted")
	}
}

func TestProtocolErrorNamesAndBounds(t *testing.T) {
	long := strings.Repeat("x", 200)
	err := NewProtocolError(ErrorPolicyDenied, long, true)
	if len(err.Detail) != 128 || !err.Retryable || err.Error() != err.Detail {
		t.Fatalf("bounded protocol error = %#v", err)
	}
	if (*ProtocolError)(nil).Error() != "" {
		t.Fatal("nil protocol error should have empty Error string")
	}
	for code := ErrorInvalidFrame; code <= ErrorInternal; code++ {
		if NewProtocolError(code, "", false).Error() == "" {
			t.Fatalf("error code %d has no stable name", code)
		}
	}
	if NewProtocolError(ErrorCodeUnspecified, "", false).Error() != "protocol error" {
		t.Fatal("unknown error code name changed")
	}
}

func TestProtocolFieldValidatorsRejectMalformedValues(t *testing.T) {
	validLimits := Limits{MaxFrameBytes: 4096, MaxRecordBytes: 2048, MaxUDPBytes: 1024, MaxStreams: 4}
	validCaps := []Capability{CapabilityErrorsV1, CapabilityLimitsV1}
	if err := ValidateHello(Hello{AgentID: "edge-a", Capabilities: validCaps, Limits: validLimits}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []Hello{
		{AgentID: "", Capabilities: validCaps, Limits: validLimits},
		{AgentID: "edge/a", Capabilities: validCaps, Limits: validLimits},
		{AgentID: strings.Repeat("x", MaxAgentIDBytes+1), Capabilities: validCaps, Limits: validLimits},
		{AgentID: "edge-a", Capabilities: []Capability{CapabilityLimitsV1, CapabilityErrorsV1}, Limits: validLimits},
		{AgentID: "edge-a", Capabilities: append([]Capability(nil), make([]Capability, MaxCapabilities+1)...), Limits: validLimits},
		{AgentID: "edge-a", Capabilities: validCaps, Limits: Limits{}},
	} {
		if err := ValidateHello(value); err == nil {
			t.Fatalf("malformed hello %#v was accepted", value)
		}
	}
	if err := ValidateChallenge(Challenge{Nonce: make([]byte, 32), Capabilities: validCaps, Limits: validLimits}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChallenge(Challenge{Nonce: make([]byte, 31), Capabilities: validCaps, Limits: validLimits}); err == nil {
		t.Fatal("short challenge nonce was accepted")
	}
	if err := ValidateAuth(Auth{MAC: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAuth(Auth{MAC: make([]byte, 31)}); err == nil {
		t.Fatal("short authentication MAC was accepted")
	}

	validMapping := TunnelRegistration{Name: "web", Protocol: "tcp", GatewayPort: 443, Profile: "standard"}
	if err := ValidateRegister(Register{Mappings: []TunnelRegistration{validMapping}}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []Register{
		{Mappings: []TunnelRegistration{{Name: "", Protocol: "tcp", GatewayPort: 443, Profile: "standard"}}},
		{Mappings: []TunnelRegistration{{Name: "web", Protocol: "icmp", GatewayPort: 443, Profile: "standard"}}},
		{Mappings: []TunnelRegistration{{Name: "web", Protocol: "tcp", GatewayPort: 0, Profile: "standard"}}},
		{Mappings: []TunnelRegistration{{Name: "web", Protocol: "tcp", GatewayPort: 443, Profile: "unknown"}}},
		{Mappings: []TunnelRegistration{{Name: strings.Repeat("x", MaxMappingNameBytes+1), Protocol: "tcp", GatewayPort: 443, Profile: "standard"}}},
		{Mappings: make([]TunnelRegistration, MaxMappings+1)},
	} {
		if err := ValidateRegister(value); err == nil {
			t.Fatalf("malformed register %#v was accepted", value)
		}
	}

	if err := ValidateOpenProxy(OpenProxy{Network: "tcp", Address: "example.com", Port: 443, Profile: "standard"}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []OpenProxy{
		{Network: "icmp", Address: "example.com", Port: 443, Profile: "standard"},
		{Network: "tcp", Address: "example.com", Port: 0, Profile: "standard"},
		{Network: "tcp", Address: "", Port: 443, Profile: "standard"},
		{Network: "tcp", Address: "example.com\nHost: evil", Port: 443, Profile: "standard"},
		{Network: "tcp", Address: strings.Repeat("x", MaxEndpointBytes+1), Port: 443, Profile: "standard"},
		{Network: "tcp", Address: "example.com", Port: 443, Profile: "unknown"},
	} {
		if err := ValidateOpenProxy(value); err == nil {
			t.Fatalf("malformed proxy open %#v was accepted", value)
		}
	}
	if err := ValidateOpenReverse(OpenReverse{Name: "web", Protocol: "udp", Profile: "balanced"}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []OpenReverse{
		{Name: "web/name", Protocol: "tcp", Profile: "standard"},
		{Name: strings.Repeat("x", MaxMappingNameBytes+1), Protocol: "tcp", Profile: "standard"},
		{Name: "web", Protocol: "icmp", Profile: "standard"},
		{Name: "web", Protocol: "tcp", Profile: "unknown"},
	} {
		if err := ValidateOpenReverse(value); err == nil {
			t.Fatalf("malformed reverse open %#v was accepted", value)
		}
	}
}

func TestMessagePointerAndTypeMismatchVariants(t *testing.T) {
	cases := []struct {
		name  string
		typ   byte
		value any
		nilV  any
		wrong any
	}{
		{name: "hello", typ: TypeHello, value: Hello{}, nilV: (*Hello)(nil), wrong: &Auth{}},
		{name: "challenge", typ: TypeChallenge, value: Challenge{}, nilV: (*Challenge)(nil), wrong: &Auth{}},
		{name: "auth", typ: TypeAuth, value: Auth{}, nilV: (*Auth)(nil), wrong: &AuthResult{}},
		{name: "auth-result", typ: TypeAuthOK, value: AuthResult{}, nilV: (*AuthResult)(nil), wrong: &Auth{}},
		{name: "register", typ: TypeRegister, value: Register{}, nilV: (*Register)(nil), wrong: &Auth{}},
		{name: "register-result", typ: TypeRegisterResult, value: RegisterResult{}, nilV: (*RegisterResult)(nil), wrong: &Auth{}},
		{name: "open-proxy", typ: TypeOpenProxy, value: OpenProxy{}, nilV: (*OpenProxy)(nil), wrong: &Auth{}},
		{name: "open-reverse", typ: TypeOpenReverse, value: OpenReverse{}, nilV: (*OpenReverse)(nil), wrong: &Auth{}},
		{name: "open-result", typ: TypeOpenOK, value: OpenResult{}, nilV: (*OpenResult)(nil), wrong: &Auth{}},
		{name: "data", typ: TypeData, value: Data{}, nilV: (*Data)(nil), wrong: &Auth{}},
		{name: "error", typ: TypeError, value: ProtocolError{}, nilV: (*ProtocolError)(nil), wrong: &Auth{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pointer := tc.value
			frame, err := MessageFrame(tc.typ, 0, pointerForMessage(tc.typ, pointer))
			if err != nil {
				t.Fatal(err)
			}
			if err := DecodeMessage(frame, tc.wrong); err == nil {
				t.Fatal("type-mismatched decode was accepted")
			}
			if _, err := MessageFrame(tc.typ, 0, tc.nilV); err == nil {
				t.Fatal("nil typed payload was accepted")
			}
		})
	}

	for _, typ := range []byte{TypePing, TypePong} {
		if _, err := MessageFrame(typ, 0, nil); err != nil {
			t.Fatalf("nil empty payload for type %d failed: %v", typ, err)
		}
		if _, err := MessageFrame(typ, 0, (*Hello)(nil)); err == nil {
			t.Fatalf("wrong empty payload for type %d was accepted", typ)
		}
		frame, err := MessageFrame(typ, 0, &struct{}{})
		if err != nil {
			t.Fatal(err)
		}
		if err := DecodeMessage(frame, &Hello{}); err == nil {
			t.Fatalf("empty payload type %d accepted wrong target", typ)
		}
	}

	var malformed frameEncoder
	malformed.string("tcp")
	malformed.string("example.com")
	malformed.uvarint(65536)
	malformed.string("standard")
	if err := DecodeMessage(Frame{Version: Version, Type: TypeOpenProxy, Payload: malformed.buf}, &OpenProxy{}); err == nil {
		t.Fatal("out-of-range open proxy port was accepted")
	}
}

func pointerForMessage(typ byte, value any) any {
	switch typ {
	case TypeHello:
		v := value.(Hello)
		return &v
	case TypeChallenge:
		v := value.(Challenge)
		return &v
	case TypeAuth:
		v := value.(Auth)
		return &v
	case TypeAuthOK:
		v := value.(AuthResult)
		return &v
	case TypeRegister:
		v := value.(Register)
		return &v
	case TypeRegisterResult:
		v := value.(RegisterResult)
		return &v
	case TypeOpenProxy:
		v := value.(OpenProxy)
		return &v
	case TypeOpenReverse:
		v := value.(OpenReverse)
		return &v
	case TypeOpenOK:
		v := value.(OpenResult)
		return &v
	case TypeData:
		v := value.(Data)
		return &v
	case TypeError:
		v := value.(ProtocolError)
		return &v
	default:
		return value
	}
}
