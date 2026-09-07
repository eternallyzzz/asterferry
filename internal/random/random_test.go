package random

import "testing"

func TestUint16nBounds(t *testing.T) {
	for _, n := range []uint32{1, 2, 3, 255, 1 << 16} {
		for i := 0; i < 32; i++ {
			value, err := Uint16n(n)
			if err != nil || value >= n {
				t.Fatalf("Uint16n(%d) = %d, %v", n, value, err)
			}
		}
	}
	for _, n := range []uint32{0, 1<<16 + 1} {
		if _, err := Uint16n(n); err == nil {
			t.Fatalf("Uint16n(%d) should reject the range", n)
		}
	}
}
