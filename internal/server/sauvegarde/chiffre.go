// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

// Chiffrement d'une archive (SPEC.md § 16.9) :
//
//	en-tête : "MIRSAV1\n" | sel aléatoire (32 octets)
//	puis segments de 64 Kio de données, chacun chiffré en AES-256-GCM
//	(+16 octets d'étiquette). Nonce = numéro du segment (11 octets) + 1 octet
//	« dernier segment ».
//
// La clé de chaque archive est dérivée (HKDF-SHA256) de la clé des secrets du
// serveur et du sel : deux archives n'ont jamais la même clé. Un segment
// modifié, déplacé, supprimé, ou une archive coupée, sont détectés.
const (
	magie         = "MIRSAV1\n"
	tailleSel     = 32
	tailleSegment = 64 << 10
	infoHKDF      = "mirador-sauvegarde-v1"
)

// ErrArchive : clé fausse, archive modifiée ou incomplète.
var ErrArchive = errors.New("archive illisible : mauvaise clé, archive modifiée ou incomplète")

func aeadPour(cle, sel []byte) (cipher.AEAD, error) {
	if len(cle) != 32 {
		return nil, errors.New("clé des secrets : 32 octets attendus")
	}
	k, err := hkdf.Key(sha256.New, cle, sel, infoHKDF, 32)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func nonce(n uint64, dernier bool) []byte {
	var v [12]byte
	binary.BigEndian.PutUint64(v[3:11], n)
	if dernier {
		v[11] = 1
	}
	return v[:]
}

// ecrivain chiffre ce qu'on lui écrit.
type ecrivain struct {
	w      io.Writer
	aead   cipher.AEAD
	tampon []byte
	n      uint64
	ferme  bool
}

// NouvelEcrivain écrit l'en-tête puis chiffre les données. Close écrit le
// dernier segment (obligatoire : sans lui, l'archive est refusée).
func NouvelEcrivain(w io.Writer, cle []byte) (io.WriteCloser, error) {
	sel := make([]byte, tailleSel)
	if _, err := rand.Read(sel); err != nil {
		return nil, err
	}
	aead, err := aeadPour(cle, sel)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(append([]byte(magie), sel...)); err != nil {
		return nil, err
	}
	return &ecrivain{w: w, aead: aead, tampon: make([]byte, 0, tailleSegment)}, nil
}

func (e *ecrivain) Write(p []byte) (int, error) {
	if e.ferme {
		return 0, errors.New("écriture après fermeture")
	}
	total := len(p)
	for len(p) > 0 {
		// Le segment n'est scellé que quand on sait qu'un autre suit.
		if len(e.tampon) == tailleSegment {
			if err := e.sceller(false); err != nil {
				return total - len(p), err
			}
		}
		k := copy(e.tampon[len(e.tampon):tailleSegment], p)
		e.tampon = e.tampon[:len(e.tampon)+k]
		p = p[k:]
	}
	return total, nil
}

func (e *ecrivain) sceller(dernier bool) error {
	sortie := e.aead.Seal(nil, nonce(e.n, dernier), e.tampon, nil)
	e.n++
	e.tampon = e.tampon[:0]
	_, err := e.w.Write(sortie)
	return err
}

func (e *ecrivain) Close() error {
	if e.ferme {
		return nil
	}
	e.ferme = true
	return e.sceller(true)
}

// lecteur déchiffre segment par segment.
type lecteur struct {
	r       *bufio.Reader
	aead    cipher.AEAD
	clair   []byte
	chiffre []byte
	n       uint64
	fini    bool
}

// NouveauLecteur vérifie l'en-tête et déchiffre au fil de la lecture. Toute
// anomalie (clé, modification, troncature) donne ErrArchive.
func NouveauLecteur(r io.Reader, cle []byte) (io.Reader, error) {
	br := bufio.NewReaderSize(r, tailleSegment+64)
	tete := make([]byte, len(magie)+tailleSel)
	if _, err := io.ReadFull(br, tete); err != nil || string(tete[:len(magie)]) != magie {
		return nil, errors.New("ce fichier n'est pas une sauvegarde de mirador")
	}
	aead, err := aeadPour(cle, tete[len(magie):])
	if err != nil {
		return nil, err
	}
	return &lecteur{r: br, aead: aead, chiffre: make([]byte, tailleSegment+16)}, nil
}

func (l *lecteur) Read(p []byte) (int, error) {
	for len(l.clair) == 0 {
		if l.fini {
			return 0, io.EOF
		}
		n, err := io.ReadFull(l.r, l.chiffre)
		switch {
		case errors.Is(err, io.ErrUnexpectedEOF), err == nil:
		case errors.Is(err, io.EOF):
			return 0, ErrArchive // plus rien alors que le dernier segment n'est pas venu
		default:
			return 0, err
		}
		_, apres := l.r.Peek(1)
		dernier := errors.Is(apres, io.EOF)
		clair, e := l.aead.Open(l.clair[:0], nonce(l.n, dernier), l.chiffre[:n], nil)
		if e != nil {
			return 0, ErrArchive
		}
		l.n++
		l.clair, l.fini = clair, dernier
	}
	k := copy(p, l.clair)
	l.clair = l.clair[k:]
	return k, nil
}
