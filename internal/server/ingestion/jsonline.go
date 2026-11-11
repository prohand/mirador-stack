// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/gzip"

	"mirador-stack/internal/server/stockage"
)

// LimitesLogs bornent une requête de logs.
type LimitesLogs struct {
	MaxDecompresse int // octets après décompression (protection « bombe gzip »)
	MaxLignes      int // lignes par requête
	MaxLigne       int // octets d'une ligne JSON (au-delà : ligne ignorée)
	MaxMessage     int // octets du message (au-delà : tronqué)
	MaxChamps      int // champs par ligne (au-delà : ignorés)
	MaxNomChamp    int
	MaxValeurChamp int // au-delà : champ ignoré
}

// LimitesLogsDefaut : l'agent coupe les messages à 16 Kio et envoie des lots
// de 512 Kio ; les échappements JSON peuvent multiplier la taille d'une ligne.
func LimitesLogsDefaut() LimitesLogs {
	return LimitesLogs{
		MaxDecompresse: 4 << 20,
		MaxLignes:      20_000,
		MaxLigne:       128 << 10,
		MaxMessage:     64 << 10,
		MaxChamps:      32,
		MaxNomChamp:    128,
		MaxValeurChamp: 2048,
	}
}

// DecoderJsonline lit le format « jsonline » de VictoriaLogs envoyé par
// l'agent : un objet JSON par ligne, avec _time (RFC 3339), _msg et des
// champs texte. Le corps peut être compressé en gzip.
//
// Lignes ignorées (et comptées) : JSON invalide, trop longue, sans _msg.
// Une heure absente ou illisible est remplacée par l'heure de réception.
func DecoderJsonline(corps io.Reader, compresse bool, lim LimitesLogs, reception time.Time) (lignes []stockage.Ligne, ignorees int, err error) {
	src := corps
	if compresse {
		gz, err := gzip.NewReader(corps)
		if err != nil {
			return nil, 0, fmt.Errorf("%w : gzip : %v", ErrFormat, err)
		}
		defer gz.Close()
		src = gz
	}
	borne := &lecteurBorne{r: src, reste: int64(lim.MaxDecompresse)}
	br := bufio.NewReaderSize(borne, lim.MaxLigne)
	for {
		ligne, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Ligne trop longue : on saute le reste jusqu'à la fin de ligne.
			ignorees++
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			if err == nil {
				continue
			}
			ligne = nil
		}
		if len(bytes.TrimSpace(ligne)) > 0 {
			if len(lignes)+ignorees >= lim.MaxLignes {
				return nil, 0, fmt.Errorf("%w : plus de %d lignes", ErrTropGros, lim.MaxLignes)
			}
			if l, ok := decoderLigne(ligne, lim, reception); ok {
				lignes = append(lignes, l)
			} else {
				ignorees++
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return lignes, ignorees, nil
		case errors.Is(err, errTropDecompresse):
			return nil, 0, fmt.Errorf("%w : plus de %d octets décompressés", ErrTropGros, lim.MaxDecompresse)
		default:
			return nil, 0, fmt.Errorf("%w : %w", ErrFormat, err)
		}
	}
}

// decoderLigne lit un objet JSON plat. Valeurs acceptées : texte, nombre,
// booléen (convertis en texte) ; objets et tableaux ignorés.
func decoderLigne(b []byte, lim LimitesLogs, reception time.Time) (stockage.Ligne, bool) {
	var brut map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&brut); err != nil || d.More() {
		return stockage.Ligne{}, false
	}
	msg, ok := brut["_msg"].(string)
	if !ok {
		return stockage.Ligne{}, false
	}
	if len(msg) > lim.MaxMessage {
		msg = strings.ToValidUTF8(msg[:lim.MaxMessage], "")
	}
	l := stockage.Ligne{T: reception.UnixMilli(), Msg: msg, Champs: make(map[string]string, min(len(brut), lim.MaxChamps))}
	if t, ok := brut["_time"].(string); ok {
		if h, err := time.Parse(time.RFC3339Nano, t); err == nil {
			l.T = h.UnixMilli()
		}
	}
	for nom, v := range brut {
		if len(l.Champs) >= lim.MaxChamps {
			break
		}
		if strings.HasPrefix(nom, "_") || len(nom) > lim.MaxNomChamp || !NomChampValide(nom) {
			continue // _time, _msg, _stream… : champs réservés
		}
		var val string
		switch x := v.(type) {
		case string:
			val = x
		case json.Number:
			val = x.String()
		case bool:
			val = fmt.Sprint(x)
		default:
			continue
		}
		if val == "" || len(val) > lim.MaxValeurChamp || !utf8.ValidString(val) {
			continue
		}
		l.Champs[nom] = val
	}
	return l, true
}

// NomChampValide : lettres, chiffres, « _ », « . », « - ».
func NomChampValide(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || c == '.' || c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// lecteurBorne renvoie une erreur au-delà de « reste » octets (contrairement
// à io.LimitReader qui s'arrête sans rien dire).
type lecteurBorne struct {
	r     io.Reader
	reste int64
}

var errTropDecompresse = errors.New("trop de données décompressées")

func (l *lecteurBorne) Read(p []byte) (int, error) {
	if l.reste <= 0 {
		return 0, errTropDecompresse
	}
	if int64(len(p)) > l.reste {
		p = p[:l.reste]
	}
	n, err := l.r.Read(p)
	l.reste -= int64(n)
	return n, err
}
