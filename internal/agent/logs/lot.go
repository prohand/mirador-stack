// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import (
	"bytes"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/gzip"
)

// Lot accumule les lignes au format « jsonline » de VictoriaLogs (un objet
// JSON par ligne), puis les compresse en gzip pour l'envoi.
//
// Champs envoyés (mêmes étiquettes qu'Alloy aujourd'hui, SPEC.md § 4.3) :
//
//	journal : _time, _msg, host, job="journal", site, env, unit, level, ident
//	docker  : _time, _msg, host, job="docker", site, env, container, stream
//
// Le JSON est écrit à la main (pas de réflexion), dans un tampon réutilisé.
type Lot struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	lignes    int
	commun    []byte // ,"host":"…","job":"journal","site":"…","env":"…"
	tailleMax int
	gz        *gzip.Writer
	sortie    bytes.Buffer
}

// tailleMaxMessage : au-delà, le message est coupé.
const tailleMaxMessage = 16 << 10

// NewLot prépare un lot. tailleMax : taille non compressée qui déclenche un
// envoi (512 Kio, comme Alloy).
func NewLot(host, job, site, env string, tailleMax int) *Lot {
	l := &Lot{tailleMax: tailleMax}
	var c []byte
	for _, kv := range [][2]string{{"host", host}, {"job", job}, {"site", site}, {"env", env}} {
		if kv[1] == "" {
			continue // site / env non donnés : mirador-server les ajoute (fiche machine)
		}
		c = append(c, ',')
		c = ajouterChaine(c, kv[0])
		c = append(c, ':')
		c = ajouterChaine(c, kv[1])
	}
	l.commun = c
	l.gz, _ = gzip.NewWriterLevel(&l.sortie, gzip.BestSpeed)
	return l
}

// Champs : étiquettes propres à une ligne (nom, valeur) ; une valeur vide
// n'est pas envoyée. Passé par valeur : pas d'allocation.
type Champs [3][2]string

// Ajouter écrit une ligne dans le lot.
func (l *Lot) Ajouter(t time.Time, msg string, champs Champs) {
	if len(msg) > tailleMaxMessage {
		msg = msg[:tailleMaxMessage]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buf.AvailableBuffer()
	b = append(b, `{"_time":`...)
	b = append(b, '"')
	b = t.UTC().AppendFormat(b, time.RFC3339Nano)
	b = append(b, `","_msg":`...)
	b = ajouterChaine(b, msg)
	b = append(b, l.commun...)
	for _, kv := range champs {
		if kv[0] == "" || kv[1] == "" {
			continue
		}
		b = append(b, ',')
		b = ajouterChaine(b, kv[0])
		b = append(b, ':')
		b = ajouterChaine(b, kv[1])
	}
	b = append(b, "}\n"...)
	l.buf.Write(b)
	l.lignes++
}

// Plein dit si le lot a atteint sa taille d'envoi.
func (l *Lot) Plein() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Len() >= l.tailleMax
}

// Vider compresse le contenu, remet le lot à zéro et renvoie les octets
// compressés (nil si le lot est vide) et le nombre de lignes.
func (l *Lot) Vider() ([]byte, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lignes == 0 {
		return nil, 0
	}
	l.sortie.Reset()
	l.gz.Reset(&l.sortie)
	_, _ = l.gz.Write(l.buf.Bytes())
	_ = l.gz.Close()
	n := l.lignes
	l.buf.Reset()
	l.lignes = 0
	return bytes.Clone(l.sortie.Bytes()), n
}

// ajouterChaine écrit s en chaîne JSON : guillemets, échappements, et
// caractères invalides remplacés (un log peut contenir n'importe quoi).
func ajouterChaine(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				b = append(b, '\\', c)
			case c == '\n':
				b = append(b, '\\', 'n')
			case c == '\r':
				b = append(b, '\\', 'r')
			case c == '\t':
				b = append(b, '\\', 't')
			case c < 0x20 || c == 0x7f:
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			default:
				b = append(b, c)
			}
			i++
			continue
		}
		r, taille := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && taille == 1 {
			b = append(b, `�`...)
		} else {
			b = append(b, s[i:i+taille]...)
		}
		i += taille
	}
	return append(b, '"')
}
