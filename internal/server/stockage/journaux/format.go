// SPDX-License-Identifier: GPL-3.0-or-later

package journaux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"mirador-stack/internal/server/stockage"
)

// Format d'un bloc dans un fichier de segment :
//
//	"MLB1" | longueur de l'en-tête (uint32) | en-tête | CRC32C de l'en-tête (uint32) | données zstd
//
// En-tête (entiers en varint) :
//
//	tmin, tmax (ms) | nombre de lignes | taille compressée | taille brute | CRC32C des données
//	champs indexés : nombre, puis pour chacun : nom, nombre de valeurs (0 = trop pour être
//	listées), valeurs
//	filtre de Bloom des mots des messages : nombre de fonctions, taille, octets
//
// Données (avant compression), pour chaque ligne :
//
//	T - tmin | message | nombre de champs | (nom, valeur)…
//
// Les chaînes sont précédées de leur longueur. Les CRC détectent un bloc
// abîmé (arrêt brutal pendant l'écriture, disque) : il est écarté.
const magie = "MLB1"

var crc = crc32.MakeTable(crc32.Castagnoli)

// Limites de lecture d'un bloc (fichier modifié ou abîmé : pas d'allocation folle).
const (
	maxEntete = 1 << 20
	maxBrut   = 4 << 20
)

var errBloc = errors.New("bloc de logs abîmé")

// champsIndexes : étiquettes dont les valeurs sont listées dans l'en-tête de
// chaque bloc (SPEC § 7.2 : machine, unité, niveau…). Au-delà de
// maxValeursIndex valeurs différentes dans un bloc, la liste n'est pas
// gardée (le bloc sera lu).
// « groupe » ajouté le 11/11 : chaque en-tête liste ses propres champs, les
// anciens blocs restent lisibles (un champ absent de l'en-tête = bloc lu).
var champsIndexes = []string{"host", "job", "level", "unit", "container", "site", "env", "groupe"}

const maxValeursIndex = 64

type entete struct {
	tmin, tmax   int64
	lignes       int
	compresse    int
	brut         int
	crcDonnees   uint32
	index        map[string][]string // nil : trop de valeurs
	bloomK       int
	bloom        []byte
	tailleEntete int // octets avant les données (magie, longueur, en-tête, CRC)
	tailleTotale int // tailleEntete + compresse
}

// --- Écriture ----------------------------------------------------------------------

func ajouterUvarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

func ajouterChaine(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// encoderLignes sérialise les lignes (non compressé).
func encoderLignes(b []byte, lignes []stockage.Ligne, tmin int64) []byte {
	for i := range lignes {
		l := &lignes[i]
		b = ajouterUvarint(b, uint64(l.T-tmin)) // #nosec G115 -- T ≥ tmin
		b = ajouterChaine(b, l.Msg)
		noms := make([]string, 0, len(l.Champs))
		for k := range l.Champs {
			noms = append(noms, k)
		}
		sort.Strings(noms) // ordre stable : meilleure compression
		b = ajouterUvarint(b, uint64(len(noms)))
		for _, k := range noms {
			b = ajouterChaine(b, k)
			b = ajouterChaine(b, l.Champs[k])
		}
	}
	return b
}

// encoderEntete écrit l'en-tête complet (magie, longueur, en-tête, CRC).
func encoderEntete(e *entete) []byte {
	var h []byte
	h = binary.AppendVarint(h, e.tmin)
	h = binary.AppendVarint(h, e.tmax)
	h = ajouterUvarint(h, uint64(e.lignes))    // #nosec G115 -- ≥ 0
	h = ajouterUvarint(h, uint64(e.compresse)) // #nosec G115 -- ≥ 0
	h = ajouterUvarint(h, uint64(e.brut))      // #nosec G115 -- ≥ 0
	h = ajouterUvarint(h, uint64(e.crcDonnees))
	h = ajouterUvarint(h, uint64(len(champsIndexes)))
	for _, nom := range champsIndexes {
		h = ajouterChaine(h, nom)
		vals, ok := e.index[nom]
		if !ok || vals == nil {
			h = ajouterUvarint(h, 0)
			continue
		}
		h = ajouterUvarint(h, uint64(len(vals))+1) // +1 : distinguer « aucune » de « trop »
		for _, v := range vals {
			h = ajouterChaine(h, v)
		}
	}
	h = ajouterUvarint(h, uint64(e.bloomK)) // #nosec G115 -- petit
	h = ajouterChaine(h, string(e.bloom))
	out := make([]byte, 0, len(h)+12)
	out = append(out, magie...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(h))) // #nosec G115 -- borné par maxEntete
	out = append(out, h...)
	return binary.LittleEndian.AppendUint32(out, crc32.Checksum(h, crc))
}

// --- Lecture -----------------------------------------------------------------------

type lecteur struct {
	b   []byte
	err error
}

func (l *lecteur) uvarint() uint64 {
	if l.err != nil {
		return 0
	}
	v, n := binary.Uvarint(l.b)
	if n <= 0 {
		l.err = errBloc
		return 0
	}
	l.b = l.b[n:]
	return v
}

func (l *lecteur) varint() int64 {
	if l.err != nil {
		return 0
	}
	v, n := binary.Varint(l.b)
	if n <= 0 {
		l.err = errBloc
		return 0
	}
	l.b = l.b[n:]
	return v
}

func (l *lecteur) entier(max int) int {
	v := l.uvarint()
	if v > uint64(max) { // #nosec G115 -- max ≥ 0
		l.err = errBloc
		return 0
	}
	return int(v) // #nosec G115 -- ≤ max
}

func (l *lecteur) chaine() string {
	v := l.uvarint() // lire la longueur AVANT de la comparer au reste du tampon
	if l.err != nil {
		return ""
	}
	if v > uint64(len(l.b)) {
		l.err = errBloc
		return ""
	}
	n := int(v) // #nosec G115 -- ≤ len(l.b)
	s := string(l.b[:n])
	l.b = l.b[n:]
	return s
}

// lireEntete lit un en-tête à la position courante de r. io.EOF si fin propre
// du fichier ; errBloc si l'en-tête est tronqué ou abîmé.
func lireEntete(r io.Reader) (*entete, error) {
	var debut [8]byte
	if _, err := io.ReadFull(r, debut[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, errBloc
	}
	if string(debut[:4]) != magie {
		return nil, errBloc
	}
	n := binary.LittleEndian.Uint32(debut[4:])
	if n > maxEntete {
		return nil, errBloc
	}
	h := make([]byte, int(n)+4)
	if _, err := io.ReadFull(r, h); err != nil {
		return nil, errBloc
	}
	if crc32.Checksum(h[:n], crc) != binary.LittleEndian.Uint32(h[n:]) {
		return nil, errBloc
	}
	e, err := decoderEntete(h[:n])
	if err != nil {
		return nil, err
	}
	e.tailleEntete = 8 + int(n) + 4
	e.tailleTotale = e.tailleEntete + e.compresse
	return e, nil
}

func decoderEntete(h []byte) (*entete, error) {
	l := &lecteur{b: h}
	e := &entete{index: map[string][]string{}}
	e.tmin, e.tmax = l.varint(), l.varint()
	e.lignes = l.entier(1 << 24)
	e.compresse = l.entier(maxBrut)
	e.brut = l.entier(maxBrut)
	e.crcDonnees = uint32(l.entier(1<<32 - 1)) // #nosec G115 -- ≤ 2^32-1
	nc := l.entier(64)
	for i := 0; i < nc && l.err == nil; i++ {
		nom := l.chaine()
		nv := l.entier(maxValeursIndex + 1)
		if nv == 0 {
			e.index[nom] = nil
			continue
		}
		vals := make([]string, 0, nv-1)
		for j := 0; j < nv-1 && l.err == nil; j++ {
			vals = append(vals, l.chaine())
		}
		e.index[nom] = vals
	}
	e.bloomK = l.entier(16)
	e.bloom = []byte(l.chaine())
	if l.err != nil || e.tmax < e.tmin {
		return nil, errBloc
	}
	return e, nil
}

// decoderLignes relit les lignes d'un bloc décompressé.
func decoderLignes(b []byte, e *entete, garder func(*stockage.Ligne) bool) error {
	l := &lecteur{b: b}
	for i := 0; i < e.lignes; i++ {
		var ln stockage.Ligne
		ln.T = e.tmin + int64(l.uvarint()) // #nosec G115 -- écart borné
		ln.Msg = l.chaine()
		nc := l.entier(256)
		if nc > 0 {
			ln.Champs = make(map[string]string, nc)
		}
		for j := 0; j < nc && l.err == nil; j++ {
			k := l.chaine()
			ln.Champs[k] = l.chaine()
		}
		if l.err != nil {
			return l.err
		}
		if !garder(&ln) {
			return nil
		}
	}
	return nil
}

// --- Mots et filtre de Bloom -------------------------------------------------------

// mots découpe un texte en mots (lettres et chiffres), en minuscules,
// de 2 à 64 caractères.
func mots(s string, f func(string)) {
	debut := -1
	for i := 0; i <= len(s); {
		r, n := utf8.RuneError, 1
		if i < len(s) {
			r, n = utf8.DecodeRuneInString(s[i:])
		}
		estMot := i < len(s) && (unicode.IsLetter(r) || unicode.IsDigit(r))
		switch {
		case estMot && debut < 0:
			debut = i
		case !estMot && debut >= 0:
			if m := s[debut:i]; len(m) >= 2 && len(m) <= 64 {
				f(strings.ToLower(m))
			}
			debut = -1
		}
		i += n
	}
}

// hachages : double hachage (FNV-1a 64 bits coupé en deux) → k positions.
func hachages(mot string) (uint32, uint32) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(mot))
	v := h.Sum64()
	return uint32(v), uint32(v>>32) | 1 // #nosec G115 -- découpage voulu
}

// nouveauBloom : 8 bits par mot (≈ 2 % de faux positifs), 5 fonctions.
func nouveauBloom(mots map[string]struct{}) (k int, b []byte) {
	n := max(len(mots), 1)
	b = make([]byte, (n*8+7)/8)
	bits := uint32(len(b) * 8) // #nosec G115 -- borné par le nombre de mots
	for m := range mots {
		h1, h2 := hachages(m)
		for i := uint32(0); i < 5; i++ {
			p := (h1 + i*h2) % bits
			b[p/8] |= 1 << (p % 8)
		}
	}
	return 5, b
}

// peutContenir : faux = le mot n'est sûrement pas dans le bloc.
func peutContenir(k int, b []byte, mot string) bool {
	if len(b) == 0 {
		return true
	}
	bits := uint32(len(b) * 8) // #nosec G115 -- borné par maxEntete
	h1, h2 := hachages(mot)
	for i := uint32(0); i < uint32(k); i++ { // #nosec G115 -- k ≤ 16
		p := (h1 + i*h2) % bits
		if b[p/8]&(1<<(p%8)) == 0 {
			return false
		}
	}
	return true
}

func erreurf(format string, a ...any) error { return fmt.Errorf("journaux : "+format, a...) }
