// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/klauspost/compress/snappy"

	"mirador-stack/internal/server/stockage"
)

// Erreurs de décodage. ErrFormat → HTTP 400 (l'agent abandonne le lot) ;
// ErrTropGros → HTTP 413.
var (
	ErrFormat   = errors.New("format invalide")
	ErrTropGros = errors.New("requête trop grosse")
)

// LimitesDecodage bornent ce qu'une requête peut faire consommer au serveur.
type LimitesDecodage struct {
	MaxDecompresse int // octets après décompression
	MaxSeries      int // séries par requête
	MaxPoints      int // points par requête (toutes séries)
	MaxLabels      int // étiquettes par série
	MaxNom         int // longueur d'un nom d'étiquette
	MaxValeur      int // longueur d'une valeur d'étiquette
}

// LimitesDecodageDefaut : un agent envoie ~600 à 1 000 séries par lot
// (~100 Kio décompressés, un lot par requête même après une coupure). 4 Mio
// laissent 40 fois cette marge ; le pire cas mesuré reste ~16 Mo de mémoire
// par requête (SPEC § 16.4).
func LimitesDecodageDefaut() LimitesDecodage {
	return LimitesDecodage{
		MaxDecompresse: 4 << 20,
		MaxSeries:      20_000,
		MaxPoints:      200_000,
		MaxLabels:      64,
		MaxNom:         256,
		MaxValeur:      2048,
	}
}

// DecoderRemoteWrite décode un WriteRequest Prometheus (remote write 1.0,
// protobuf compressé en snappy). Seuls les champs utiles sont lus ; les autres
// (exemplaires, histogrammes, métadonnées) sont sautés.
//
// Une erreur de structure rejette toute la requête. Une série mal formée
// (étiquette invalide, trop d'étiquettes…) est seulement ignorée et comptée,
// pour ne pas perdre les autres.
func DecoderRemoteWrite(corps []byte, lim LimitesDecodage) (series []stockage.Serie, ignorees int, err error) {
	n, err := snappy.DecodedLen(corps)
	if err != nil {
		return nil, 0, fmt.Errorf("%w : snappy : %v", ErrFormat, err)
	}
	if n > lim.MaxDecompresse {
		return nil, 0, fmt.Errorf("%w : %d octets décompressés (max %d)", ErrTropGros, n, lim.MaxDecompresse)
	}
	brut, err := snappy.Decode(make([]byte, n), corps)
	if err != nil {
		return nil, 0, fmt.Errorf("%w : snappy : %v", ErrFormat, err)
	}
	return decoderWriteRequest(brut, lim)
}

// decoderWriteRequest : message WriteRequest { repeated TimeSeries timeseries = 1; }
func decoderWriteRequest(b []byte, lim LimitesDecodage) ([]stockage.Serie, int, error) {
	l := lecteur{b: b}
	var series []stockage.Serie
	ignorees, points := 0, 0
	textes := make(interneur, 256)
	for !l.fini() {
		champ, typ, err := l.tag()
		if err != nil {
			return nil, 0, err
		}
		if champ != 1 || typ != typeLongueur {
			if err := l.sauter(typ); err != nil {
				return nil, 0, err
			}
			continue
		}
		contenu, err := l.octets()
		if err != nil {
			return nil, 0, err
		}
		if len(series)+ignorees >= lim.MaxSeries {
			return nil, 0, fmt.Errorf("%w : plus de %d séries", ErrTropGros, lim.MaxSeries)
		}
		s, valide, err := decoderSerie(contenu, lim, textes)
		if err != nil {
			return nil, 0, err
		}
		if points += len(s.Points); points > lim.MaxPoints {
			return nil, 0, fmt.Errorf("%w : plus de %d points", ErrTropGros, lim.MaxPoints)
		}
		if !valide || len(s.Points) == 0 {
			ignorees++
			continue
		}
		series = append(series, s)
	}
	return series, ignorees, nil
}

// decoderSerie : message TimeSeries { repeated Label labels = 1; repeated Sample samples = 2; }
// valide = false si la série doit être ignorée (étiquettes non conformes).
func decoderSerie(b []byte, lim LimitesDecodage, textes interneur) (s stockage.Serie, valide bool, err error) {
	l := lecteur{b: b}
	valide = true
	for !l.fini() {
		champ, typ, err := l.tag()
		if err != nil {
			return s, false, err
		}
		if typ != typeLongueur || (champ != 1 && champ != 2) {
			if err := l.sauter(typ); err != nil {
				return s, false, err
			}
			continue
		}
		contenu, err := l.octets()
		if err != nil {
			return s, false, err
		}
		if champ == 1 {
			nom, val, err := decoderLabel(contenu)
			if err != nil {
				return s, false, err
			}
			switch {
			case !valide:
			case len(s.Labels) >= lim.MaxLabels, len(nom) > lim.MaxNom, len(val) > lim.MaxValeur,
				!utf8.Valid(val):
				valide = false
			case len(val) == 0: // valeur vide = étiquette absente (règle Prometheus)
			default:
				if s.Labels == nil {
					s.Labels = make([]stockage.Label, 0, 8)
				}
				s.Labels = append(s.Labels, stockage.Label{Nom: textes.texte(nom), Valeur: textes.texte(val)})
			}
			continue
		}
		if len(s.Points) >= lim.MaxPoints {
			return s, false, fmt.Errorf("%w : plus de %d points", ErrTropGros, lim.MaxPoints)
		}
		p, err := decoderPoint(contenu)
		if err != nil {
			return s, false, err
		}
		s.Points = append(s.Points, p)
	}
	return s, valide && labelsValides(&s), nil
}

// interneur : les noms et beaucoup de valeurs d'étiquettes se répètent d'une
// série à l'autre (__name__, instance, job, cpu…). Une seule copie par
// requête : moins d'allocations et de mémoire. Taille bornée.
type interneur map[string]string

func (in interneur) texte(b []byte) string {
	if s, ok := in[string(b)]; ok { // pas d'allocation pour la recherche
		return s
	}
	s := string(b)
	if len(in) < 4096 && len(b) <= 128 {
		in[s] = s
	}
	return s
}

// labelsValides trie les étiquettes et vérifie : noms conformes, pas de
// doublon, __name__ présent et conforme, pas d'autre nom réservé (« __… »).
func labelsValides(s *stockage.Serie) bool {
	sort.Slice(s.Labels, func(i, j int) bool { return s.Labels[i].Nom < s.Labels[j].Nom })
	nomTrouve := false
	for i, lb := range s.Labels {
		if i > 0 && s.Labels[i-1].Nom == lb.Nom {
			return false
		}
		if lb.Nom == "__name__" {
			if !NomMetriqueValide(lb.Valeur) {
				return false
			}
			nomTrouve = true
			continue
		}
		if len(lb.Nom) >= 2 && lb.Nom[:2] == "__" || !NomLabelValide(lb.Nom) {
			return false
		}
	}
	return nomTrouve
}

// decoderLabel : message Label { string name = 1; string value = 2; }
func decoderLabel(b []byte) (nom, val []byte, err error) {
	l := lecteur{b: b}
	for !l.fini() {
		champ, typ, err := l.tag()
		if err != nil {
			return nil, nil, err
		}
		if typ != typeLongueur || (champ != 1 && champ != 2) {
			if err := l.sauter(typ); err != nil {
				return nil, nil, err
			}
			continue
		}
		v, err := l.octets()
		if err != nil {
			return nil, nil, err
		}
		if champ == 1 {
			nom = v
		} else {
			val = v
		}
	}
	return nom, val, nil
}

// decoderPoint : message Sample { double value = 1; int64 timestamp = 2; }
func decoderPoint(b []byte) (stockage.Point, error) {
	var p stockage.Point
	l := lecteur{b: b}
	for !l.fini() {
		champ, typ, err := l.tag()
		if err != nil {
			return p, err
		}
		switch {
		case champ == 1 && typ == type64:
			v, err := l.fixe64()
			if err != nil {
				return p, err
			}
			p.V = math.Float64frombits(v)
		case champ == 2 && typ == typeVarint:
			v, err := l.varint()
			if err != nil {
				return p, err
			}
			p.T = int64(v) // #nosec G115 -- int64 protobuf : conversion voulue (complément à deux)
		default:
			if err := l.sauter(typ); err != nil {
				return p, err
			}
		}
	}
	return p, nil
}

// NomMetriqueValide : [a-zA-Z_:][a-zA-Z0-9_:]*
func NomMetriqueValide(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// NomLabelValide : [a-zA-Z_][a-zA-Z0-9_]*
func NomLabelValide(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// --- Lecture protobuf minimale ---------------------------------------------------

// Types protobuf (« wire types ») acceptés. Les groupes (3, 4), obsolètes,
// sont refusés.
const (
	typeVarint   = 0
	type64       = 1
	typeLongueur = 2
	type32       = 5
)

type lecteur struct {
	b []byte
	i int
}

func (l *lecteur) fini() bool { return l.i >= len(l.b) }

func (l *lecteur) varint() (uint64, error) {
	v, n := binary.Uvarint(l.b[l.i:])
	if n <= 0 {
		return 0, fmt.Errorf("%w : entier protobuf tronqué", ErrFormat)
	}
	l.i += n
	return v, nil
}

func (l *lecteur) tag() (champ, typ int, err error) {
	v, err := l.varint()
	if err != nil {
		return 0, 0, err
	}
	if v>>3 == 0 || v>>3 > 1<<29 {
		return 0, 0, fmt.Errorf("%w : numéro de champ protobuf invalide", ErrFormat)
	}
	return int(v >> 3), int(v & 7), nil // #nosec G115 -- borné juste au-dessus
}

func (l *lecteur) octets() ([]byte, error) {
	n, err := l.varint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(l.b)-l.i) { // #nosec G115 -- l.i ≤ len(l.b) : différence ≥ 0
		return nil, fmt.Errorf("%w : longueur protobuf hors du message", ErrFormat)
	}
	debut := l.i
	l.i += int(n) // #nosec G115 -- n ≤ len(l.b), vérifié juste au-dessus
	return l.b[debut:l.i], nil
}

func (l *lecteur) fixe64() (uint64, error) {
	if len(l.b)-l.i < 8 {
		return 0, fmt.Errorf("%w : nombre protobuf tronqué", ErrFormat)
	}
	v := binary.LittleEndian.Uint64(l.b[l.i:])
	l.i += 8
	return v, nil
}

// sauter passe un champ inconnu.
func (l *lecteur) sauter(typ int) error {
	var err error
	switch typ {
	case typeVarint:
		_, err = l.varint()
	case type64:
		_, err = l.fixe64()
	case typeLongueur:
		_, err = l.octets()
	case type32:
		if len(l.b)-l.i < 4 {
			return fmt.Errorf("%w : nombre protobuf tronqué", ErrFormat)
		}
		l.i += 4
	default:
		return fmt.Errorf("%w : type protobuf %d non pris en charge", ErrFormat, typ)
	}
	return err
}
