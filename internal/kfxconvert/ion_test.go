package kfxconvert

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestParseIonStruct(t *testing.T) {
	// { $413: 18, $414: 48 }
	data := append([]byte(nil), ionVersionMarker...)
	data = append(data, 0xd8, 0x03, 0x9d, 0x21, 0x12, 0x03, 0x9e, 0x21, 0x30)
	values, err := parseIonValues(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("values = %d, want 1", len(values))
	}
	for id, want := range map[uint64]uint64{413: 18, 414: 48} {
		got, ok := ionUint(ionFieldValue(values[0], id))
		if !ok || got != want {
			t.Fatalf("field %d = %d, %t; want %d", id, got, ok, want)
		}
	}
}

func TestParseIonFloat(t *testing.T) {
	data := append([]byte(nil), ionVersionMarker...)
	data = append(data, 0x48)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], math.Float64bits(1.5))
	data = append(data, encoded[:]...)
	values, err := parseIonValues(data)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := ionNumber(values[0])
	if !ok || value != 1.5 {
		t.Fatalf("float = %v, %t", value, ok)
	}
}

func FuzzParseIonDoesNotPanic(f *testing.F) {
	f.Add([]byte{0xe0, 0x01, 0x00, 0xea, 0x20})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseIonValues(data)
	})
}
