package recording

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Hit est une ligne affichée qui contient le texte recherché.
type Hit struct {
	Time float64 `json:"time"` // secondes depuis le début de la session
	Line string  `json:"line"`
}

const maxHits = 200

// Search cherche query (sans tenir compte de la casse) dans le texte affiché d'un
// enregistrement asciicast. Les séquences de contrôle du terminal (couleurs,
// déplacements du curseur…) sont ignorées ; la recherche se fait ligne par ligne.
func Search(cast []byte, query string) []Hit {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	hits := []Hit{}
	var t textScanner
	sc := bufio.NewScanner(bytes.NewReader(cast))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	first := true
	for sc.Scan() {
		if first { // en-tête
			first = false
			continue
		}
		var ev []json.RawMessage
		if json.Unmarshal(sc.Bytes(), &ev) != nil || len(ev) != 3 {
			continue
		}
		var at float64
		var kind, data string
		if json.Unmarshal(ev[0], &at) != nil || json.Unmarshal(ev[1], &kind) != nil || kind != "o" ||
			json.Unmarshal(ev[2], &data) != nil {
			continue
		}
		for _, line := range t.feed(data, at) {
			if strings.Contains(strings.ToLower(line.text), q) {
				hits = append(hits, Hit{Time: line.start, Line: clip(line.text)})
				if len(hits) >= maxHits {
					return hits
				}
			}
		}
	}
	if line := t.current(); strings.Contains(strings.ToLower(line.text), q) {
		hits = append(hits, Hit{Time: line.start, Line: clip(line.text)})
	}
	return hits
}

type textLine struct {
	text  string
	start float64
}

// textScanner reconstitue grossièrement les lignes affichées : il retire les séquences
// d'échappement, gère retour chariot, retour arrière et sauts de ligne.
type textScanner struct {
	state     int // 0 normal, 1 ESC, 2 CSI, 3 OSC, 4 OSC+ESC, 5 jeu de caractères
	line      []rune
	col       int
	lineStart float64
	started   bool
}

func (t *textScanner) feed(data string, at float64) []textLine {
	var done []textLine
	for _, r := range data {
		switch t.state {
		case 1:
			switch {
			case r == '[':
				t.state = 2
			case r == ']':
				t.state = 3
			case r == '(' || r == ')' || r == '*' || r == '+':
				t.state = 5
			default:
				t.state = 0
			}
			continue
		case 2:
			if r >= 0x40 && r <= 0x7e {
				t.state = 0
			}
			continue
		case 3:
			if r == 0x07 {
				t.state = 0
			} else if r == 0x1b {
				t.state = 4
			}
			continue
		case 4:
			t.state = 0
			continue
		case 5:
			t.state = 0
			continue
		}
		switch {
		case r == 0x1b:
			t.state = 1
		case r == '\n':
			done = append(done, t.current())
			t.line, t.col, t.started = t.line[:0], 0, false
		case r == '\r':
			t.col = 0
		case r == '\b':
			if t.col > 0 {
				t.col--
			}
		case r == '\t':
			t.put(' ', at)
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
		default:
			t.put(r, at)
		}
	}
	return done
}

func (t *textScanner) put(r rune, at float64) {
	if !t.started {
		t.lineStart, t.started = at, true
	}
	if t.col < len(t.line) {
		t.line[t.col] = r // réécriture sur la même ligne (après \r ou retour arrière)
	} else {
		t.line = append(t.line, r)
	}
	t.col++
}

func (t *textScanner) current() textLine {
	return textLine{text: strings.TrimRight(string(t.line), " "), start: t.lineStart}
}

func clip(s string) string {
	r := []rune(s)
	if len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return s
}
