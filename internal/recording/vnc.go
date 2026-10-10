package recording

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"gna/internal/secure"
)

// Enregistrement VNC : le flux RFB envoyé par la cible au navigateur, à partir du
// ServerInit, horodaté. À la relecture, ce flux est réinjecté dans noVNC, qui le
// décode exactement comme en direct.
//
// Contenu (avant compression et chiffrement, même conteneur que les sessions SSH) :
//
//	"GNAVNC1\n" + en-tête JSON + "\n", puis des trames [ms u32][longueur u32][données]
const VNCMagic = "GNAVNC1\n"

// VNCHeader décrit une session VNC enregistrée.
type VNCHeader struct {
	Timestamp int64  `json:"timestamp"`
	Title     string `json:"title,omitempty"`
}

// CreateVNC ouvre un enregistrement de session VNC.
func CreateVNC(path string, id int64, box *secure.Box, h VNCHeader) (*Writer, error) {
	w, err := open(path, id, box)
	if err != nil {
		return nil, err
	}
	h.Timestamp = w.start.Unix()
	line, _ := json.Marshal(h)
	w.buf.WriteString(VNCMagic)
	w.buf.Write(append(line, '\n'))
	return w, nil
}

// Frame enregistre des octets envoyés par la cible.
func (w *Writer) Frame(p []byte) {
	if w == nil || len(p) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:], uint32(time.Since(w.start).Milliseconds()))
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(p)))
	w.buf.Write(hdr[:])
	w.buf.Write(p)
	w.written += int64(len(p)) + 8
}

// VNCFrame est une trame décodée (utile aux tests).
type VNCFrame struct {
	At   time.Duration
	Data []byte
}

// ParseVNC décode un enregistrement VNC déchiffré.
func ParseVNC(data []byte) (*VNCHeader, []VNCFrame, error) {
	if len(data) < len(VNCMagic) || string(data[:len(VNCMagic)]) != VNCMagic {
		return nil, nil, errors.New("ce n'est pas un enregistrement VNC")
	}
	data = data[len(VNCMagic):]
	nl := -1
	for i, b := range data {
		if b == '\n' {
			nl = i
			break
		}
	}
	if nl < 0 {
		return nil, nil, errors.New("en-tête VNC manquant")
	}
	h := &VNCHeader{}
	if err := json.Unmarshal(data[:nl], h); err != nil {
		return nil, nil, err
	}
	data = data[nl+1:]
	var frames []VNCFrame
	for len(data) >= 8 {
		ms := binary.BigEndian.Uint32(data[0:])
		n := int(binary.BigEndian.Uint32(data[4:]))
		if len(data) < 8+n {
			break // trame coupée par un arrêt brutal
		}
		frames = append(frames, VNCFrame{At: time.Duration(ms) * time.Millisecond, Data: data[8 : 8+n]})
		data = data[8+n:]
	}
	return h, frames, nil
}
