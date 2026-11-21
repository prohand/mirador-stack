// SPDX-License-Identifier: GPL-3.0-or-later

// Package syslog : réception syslog TLS du collecteur (SPEC § 23.14).
//
// Lecteur écrit à la main, borné et testé par fuzzing : trames RFC 5425
// (longueur en tête) ou une ligne par message ; formats RFC 5424 et
// RFC 3164 (BSD). Rien de ce qu'envoie l'émetteur ne choisit le nom de
// l'hôte : il vient de son adresse (liste des émetteurs).
package syslog

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"time"
	"unicode/utf8"
)

// TailleMaxMessage : au-delà, le message est refusé (trame) ou coupé (ligne).
const TailleMaxMessage = 8 << 10

// Message : un message syslog lu.
type Message struct {
	Niveau string    // emerg, alert, crit, err, warning, notice, info, debug
	App    string    // programme (TAG ou APP-NAME), 48 caractères au plus
	Texte  string    // message, UTF-8 valide
	Heure  time.Time // heure annoncée (zéro si absente ou illisible)
}

var niveaux = [8]string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// ErrTrame : trame illisible (la connexion est fermée).
var ErrTrame = errors.New("trame syslog illisible")

// LireTrame lit le message suivant : « LONGUEUR ESPACE MESSAGE » (RFC 5425),
// sinon une ligne terminée par « \n ». Une ligne trop longue est coupée
// (le reste est jeté) ; une trame trop longue ferme la connexion.
func LireTrame(r *bufio.Reader) ([]byte, error) {
	for {
		b, err := lireUneTrame(r)
		if err != nil || len(b) > 0 {
			return b, err
		}
		// Ligne vide (fin de ligne après une trame RFC 5425…) : ignorée.
	}
}

func lireUneTrame(r *bufio.Reader) ([]byte, error) {
	premier, err := r.Peek(1)
	if err != nil {
		return nil, err
	}
	if premier[0] >= '1' && premier[0] <= '9' {
		// Longueur en tête : 5 chiffres au plus (8 Kio).
		n := 0
		for i := 0; i < 6; i++ {
			c, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			if c == ' ' {
				break
			}
			if c < '0' || c > '9' || i == 5 {
				return nil, ErrTrame
			}
			n = n*10 + int(c-'0')
		}
		if n <= 0 || n > TailleMaxMessage {
			return nil, ErrTrame
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(r, msg); err != nil {
			return nil, err
		}
		return bytes.TrimRight(msg, "\r\n\x00"), nil
	}
	// Une ligne par message.
	// Ligne trop longue (relecture du 21/11) : le DÉBUT est gardé (8 Kio),
	// tout le reste jusqu'à la fin de ligne est jeté. Avant, un morceau trop
	// long était jeté mais les suivants gardés : la ligne disparaissait, ou
	// seule sa fin restait (et pouvait passer pour un autre message).
	var ligne []byte
	for {
		morceau, err := r.ReadSlice('\n')
		if place := TailleMaxMessage - len(ligne); place > 0 {
			ligne = append(ligne, morceau[:min(place, len(morceau))]...)
		}
		if err == nil || (len(ligne) > 0 && errors.Is(err, io.EOF)) {
			return bytes.TrimRight(ligne, "\r\n\x00"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

// Analyser lit un message RFC 5424 ou RFC 3164. Le PRI (<0> à <191>) est
// obligatoire ; le reste est lu avec indulgence (les équipements prennent
// des libertés avec les formats).
func Analyser(b []byte) (Message, bool) {
	var m Message
	if len(b) < 3 || b[0] != '<' {
		return m, false
	}
	fin := bytes.IndexByte(b[:min(len(b), 5)], '>')
	if fin < 2 {
		return m, false
	}
	pri, err := strconv.Atoi(string(b[1:fin]))
	if err != nil || pri < 0 || pri > 191 {
		return m, false
	}
	m.Niveau = niveaux[pri%8]
	reste := b[fin+1:]
	if len(reste) >= 2 && reste[0] == '1' && reste[1] == ' ' {
		analyser5424(reste[2:], &m)
	} else {
		analyser3164(reste, &m)
	}
	m.App = propre(m.App, 48)
	m.Texte = propre(m.Texte, TailleMaxMessage)
	return m, true
}

// analyser5424 : « HORODATAGE HÔTE APP PROCID MSGID [SD] MESSAGE ».
func analyser5424(b []byte, m *Message) {
	champs := [5][]byte{}
	for i := range champs {
		j := bytes.IndexByte(b, ' ')
		if j < 0 {
			champs[i], b = b, nil
			break
		}
		champs[i], b = b[:j], b[j+1:]
	}
	if t, err := time.Parse(time.RFC3339Nano, string(champs[0])); err == nil {
		m.Heure = t
	}
	if string(champs[2]) != "-" {
		m.App = string(champs[2])
	}
	// Données structurées : « - » ou une suite de [ … ] (crochets échappés
	// par « \ » à l'intérieur) ; ignorées.
	if len(b) > 0 && b[0] == '-' {
		b = b[1:]
	} else {
		for len(b) > 0 && b[0] == '[' {
			i, echappe := 1, false
			for ; i < len(b); i++ {
				if echappe {
					echappe = false
					continue
				}
				if b[i] == '\\' {
					echappe = true
				} else if b[i] == ']' {
					break
				}
			}
			if i >= len(b) {
				b = nil
				break
			}
			b = b[i+1:]
		}
	}
	b = bytes.TrimPrefix(bytes.TrimPrefix(b, []byte(" ")), []byte("\xef\xbb\xbf")) // marque UTF-8 (BOM)
	m.Texte = string(b)
}

// analyser3164 : « Mmm jj hh:mm:ss HÔTE TAG[pid]: MESSAGE », horodatage et
// hôte facultatifs.
func analyser3164(b []byte, m *Message) {
	if len(b) >= 16 && b[3] == ' ' && b[6] == ' ' && b[9] == ':' && b[12] == ':' && b[15] == ' ' {
		if t, err := time.Parse(time.Stamp, string(b[:15])); err == nil {
			m.Heure = t // sans année ni fuseau : remplacée par l'heure de réception
		}
		b = b[16:]
		// Hôte annoncé : premier mot sans « : » ni crochet (« sshd[42]: » ou
		// « kernel: » sont des TAG). Ignoré : l'hôte vient de l'adresse.
		if j := bytes.IndexByte(b, ' '); j > 0 && !bytes.ContainsAny(b[:j], ":[]") {
			b = b[j+1:]
		}
	}
	// TAG : jusqu'à « [ », « : » ou une espace (32 caractères au plus).
	i := 0
	for i < len(b) && i < 33 && b[i] != '[' && b[i] != ':' && b[i] != ' ' {
		i++
	}
	if i > 0 && i < len(b) && i <= 32 && (b[i] == '[' || b[i] == ':') {
		m.App = string(b[:i])
		b = b[i:]
		if b[0] == '[' {
			if j := bytes.IndexByte(b, ']'); j > 0 && j < 16 {
				b = b[j+1:]
			}
		}
		b = bytes.TrimPrefix(b, []byte(":"))
		b = bytes.TrimPrefix(b, []byte(" "))
	}
	m.Texte = string(b)
}

// propre : UTF-8 valide (caractères invalides remplacés), coupé à max
// octets sans couper un caractère (le remplacement peut allonger le texte :
// la coupe vient après).
func propre(s string, max int) string {
	if !utf8.ValidString(s) {
		s = string(bytes.ToValidUTF8([]byte(s), []byte("\uFFFD")))
	}
	if len(s) > max {
		i := max
		for i > 0 && !utf8.RuneStart(s[i]) {
			i--
		}
		s = s[:i]
	}
	return s
}
