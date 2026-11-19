package vncproxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestRelayWithoutClipboard(t *testing.T) {
	var in bytes.Buffer
	key := []byte{4, 1, 0, 0, 0, 0, 0, 0x61}
	in.Write(key)
	// SetEncodings : zlib, presse-papiers étendu, DesktopSize
	enc := []byte{2, 0, 0, 3}
	for _, e := range []int32{6, encExtendedClipboard, -223} {
		enc = binary.BigEndian.AppendUint32(enc, uint32(e))
	}
	in.Write(enc)
	// ClientCutText classique puis étendu
	in.Write([]byte{6, 0, 0, 0, 0, 0, 0, 5})
	in.WriteString("secre") // 5 octets annoncés
	in.Write([]byte{6, 0, 0, 0, 0xff, 0xff, 0xff, 0xfc, 0x08, 0, 0, 1})
	ptr := []byte{5, 0, 0, 10, 0, 20}
	in.Write(ptr)

	var out bytes.Buffer
	err := RelayWithoutClipboard(&out, &in, nil)
	if !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	want := append([]byte{}, key...)
	want = append(want, 2, 0, 0, 2)
	want = binary.BigEndian.AppendUint32(want, 6)
	want = binary.BigEndian.AppendUint32(want, uint32(0xffffff21)) // -223
	want = append(want, ptr...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("obtenu %x\nattendu %x", out.Bytes(), want)
	}
}

func TestRelayWithoutClipboardInconnu(t *testing.T) {
	err := RelayWithoutClipboard(io.Discard, bytes.NewReader([]byte{99, 1, 2}), nil)
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatal("un message inconnu doit interrompre le relais")
	}
}
