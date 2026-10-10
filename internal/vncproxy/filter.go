package vncproxy

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// Pseudo-encodages retirés quand le presse-papiers est interdit.
const (
	encExtendedClipboard   = int32(-1063131698) // 0xC0A1E5CE
	encExtendedMouseButton = int32(-316)        // change la taille des messages souris
)

// RelayWithoutClipboard recopie les messages du navigateur vers la cible en retirant
// tout ce qui concerne le presse-papiers (ClientCutText et presse-papiers étendu).
// Les messages sont analysés un par un : un message inconnu interrompt la session
// plutôt que de laisser passer un contenu non contrôlé. onData est appelé à chaque
// message transmis (activité de l'utilisateur).
func RelayWithoutClipboard(dst io.Writer, src io.Reader, onData func(n int)) error {
	r := bufio.NewReaderSize(src, 64<<10)
	var msg []byte
	read := func(n int) error {
		start := len(msg)
		msg = append(msg, make([]byte, n)...)
		_, err := io.ReadFull(r, msg[start:])
		return err
	}
	for {
		msg = msg[:0]
		if err := read(1); err != nil {
			return err
		}
		var err error
		switch msg[0] {
		case 0: // SetPixelFormat
			err = read(19)
		case 2: // SetEncodings : on retire le presse-papiers étendu
			if err = read(3); err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(msg[2:4]))
			if err = read(4 * n); err != nil {
				return err
			}
			kept := msg[:4]
			for i := 0; i < n; i++ {
				e := msg[4+4*i : 8+4*i]
				v := int32(binary.BigEndian.Uint32(e))
				if v != encExtendedClipboard && v != encExtendedMouseButton {
					kept = append(kept, e...)
				}
			}
			binary.BigEndian.PutUint16(kept[2:4], uint16((len(kept)-4)/4))
			msg = kept
		case 3: // FramebufferUpdateRequest
			err = read(9)
		case 4: // KeyEvent
			err = read(7)
		case 5: // PointerEvent
			err = read(5)
		case 6: // ClientCutText : lu puis ignoré
			if err = read(7); err != nil {
				return err
			}
			l := int32(binary.BigEndian.Uint32(msg[4:8]))
			if l < 0 {
				l = -l
			}
			if l > 64<<20 {
				return fmt.Errorf("message de presse-papiers trop grand")
			}
			if _, err = io.CopyN(io.Discard, r, int64(l)); err != nil {
				return err
			}
			continue
		case 150: // EnableContinuousUpdates
			err = read(9)
		case 248: // ClientFence
			if err = read(8); err != nil {
				return err
			}
			err = read(int(msg[8]))
		case 250: // xvp
			err = read(3)
		case 251: // SetDesktopSize
			if err = read(7); err != nil {
				return err
			}
			err = read(16 * int(msg[6]))
		case 255: // QEMU : événement clavier étendu
			if err = read(1); err != nil {
				return err
			}
			if msg[1] != 0 {
				return fmt.Errorf("message VNC QEMU non pris en charge (%d)", msg[1])
			}
			err = read(10)
		default:
			return fmt.Errorf("message VNC du navigateur inconnu (%d)", msg[0])
		}
		if err != nil {
			return err
		}
		if _, err := dst.Write(msg); err != nil {
			return err
		}
		if onData != nil {
			onData(len(msg))
		}
	}
}
