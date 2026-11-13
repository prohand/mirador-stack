// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mirador-stack/internal/server/stockage"
)

// Entrée InfluxDB v2 (étape 6, partie 2, SPEC.md § 23.9) : pour OPNsense, qui
// n'envoie ses métriques qu'avec Telegraf (sortie « InfluxDB v2 »).
//
// Format « line protocol », une mesure par ligne :
//
//	mesure[,tag=valeur…] champ=valeur[,champ=valeur…] [horodatage]
//
// Noms produits comme VictoriaMetrics aujourd'hui (les règles et tableaux
// actuels marchent tels quels) : une série par champ, nommée
// « mesure_champ », les tags devenant des étiquettes. Champs texte ignorés,
// booléens en 1 / 0. Lecteur écrit à la main (format simple, aucune
// dépendance), borné par LimitesDecodage, testé par fuzzing.
//
// Une ligne mal formée ou hors limites est ignorée et comptée (comme
// VictoriaMetrics) ; la requête n'est refusée que si aucune ligne n'est
// valable, ou si elle dépasse les limites globales.

// DecoderInflux lit un corps « line protocol » (gzip si compresse).
// precision : ns (défaut), us, ms ou s ; maintenant sert aux lignes sans heure.
func DecoderInflux(corps io.Reader, compresse bool, precision string, lim LimitesDecodage, maintenant time.Time) (series []stockage.Serie, ignorees int, err error) {
	versMs, err := facteurPrecision(precision)
	if err != nil {
		return nil, 0, err
	}
	src := corps
	if compresse {
		gz, err := gzip.NewReader(corps)
		if err != nil {
			return nil, 0, fmt.Errorf("%w : gzip : %v", ErrFormat, err)
		}
		defer gz.Close()
		src = gz
	}
	brut, err := io.ReadAll(&lecteurBorne{r: src, reste: int64(lim.MaxDecompresse) + 1})
	if errors.Is(err, errTropDecompresse) || len(brut) > lim.MaxDecompresse {
		return nil, 0, fmt.Errorf("%w : plus de %d octets décompressés", ErrTropGros, lim.MaxDecompresse)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("%w : %v", ErrFormat, err)
	}
	d := decodeurInflux{lim: lim, index: map[string]int{}, maintenantMs: maintenant.UnixMilli(), versMs: versMs}
	lignesBonnes := 0
	for len(brut) > 0 {
		var ligne []byte
		if i := bytes.IndexByte(brut, '\n'); i >= 0 {
			ligne, brut = brut[:i], brut[i+1:]
		} else {
			ligne, brut = brut, nil
		}
		ligne = bytes.TrimRight(ligne, "\r")
		if len(bytes.TrimSpace(ligne)) == 0 || ligne[0] == '#' {
			continue
		}
		ok, err := d.ligne(ligne)
		if err != nil {
			return nil, 0, err
		}
		if ok {
			lignesBonnes++
		} else {
			d.ignorees++
		}
	}
	if lignesBonnes == 0 && d.ignorees > 0 {
		return nil, d.ignorees, fmt.Errorf("%w : aucune ligne valable (%d ignorée(s))", ErrFormat, d.ignorees)
	}
	return d.series, d.ignorees, nil
}

// facteurPrecision : nombre par lequel diviser (ou multiplier, si négatif)
// l'horodatage reçu pour l'avoir en millisecondes.
func facteurPrecision(p string) (int64, error) {
	switch p {
	case "", "ns", "n":
		return 1_000_000, nil
	case "us", "u":
		return 1_000, nil
	case "ms":
		return 1, nil
	case "s":
		return -1000, nil
	}
	return 0, fmt.Errorf("%w : précision %q (ns, us, ms ou s)", ErrFormat, p)
}

type decodeurInflux struct {
	lim          LimitesDecodage
	series       []stockage.Serie
	index        map[string]int // clé de série → position dans series
	points       int
	ignorees     int
	maintenantMs int64
	versMs       int64
}

// ligne ajoute les séries d'une ligne. Faux : ligne ignorée. Erreur :
// limite globale dépassée (toute la requête est refusée).
func (d *decodeurInflux) ligne(b []byte) (bool, error) {
	// 1. Mesure et tags, jusqu'à la première espace non échappée.
	tete, reste, ok := couper(b, ' ', false)
	if !ok {
		return false, nil
	}
	mesure, tagsBruts, _ := couper(tete, ',', false)
	if len(mesure) == 0 {
		return false, nil
	}
	nomMesure := desechapper(mesure)
	var etiquettes []stockage.Label
	for len(tagsBruts) > 0 {
		var tag []byte
		tag, tagsBruts, _ = couper(tagsBruts, ',', false)
		k, v, ok := couper(tag, '=', false)
		if !ok || len(k) == 0 || len(v) == 0 {
			return false, nil
		}
		nom := nomPrometheus(desechapper(k), false)
		val := desechapper(v)
		if nom == "" || strings.HasPrefix(nom, "__") ||
			len(nom) > d.lim.MaxNom || len(val) > d.lim.MaxValeur || !utf8.ValidString(val) {
			return false, nil
		}
		etiquettes = append(etiquettes, stockage.Label{Nom: nom, Valeur: val})
	}
	if len(etiquettes)+1 > d.lim.MaxLabels {
		return false, nil
	}
	// 2. Champs (les guillemets des textes peuvent contenir des espaces),
	// puis horodatage facultatif.
	champs, fin, ok := couper(reste, ' ', true)
	if !ok {
		champs, fin = reste, nil
	}
	ts := d.maintenantMs
	if fin = bytes.TrimSpace(fin); len(fin) > 0 {
		n, err := strconv.ParseInt(string(fin), 10, 64)
		if err != nil {
			return false, nil
		}
		if d.versMs > 0 {
			ts = n / d.versMs
		} else {
			if n > math.MaxInt64/1000 || n < math.MinInt64/1000 {
				return false, nil
			}
			ts = n * -d.versMs
		}
	}
	type champ struct {
		nom string
		v   float64
	}
	var valeurs []champ
	for len(champs) > 0 {
		var c []byte
		c, champs, _ = couper(champs, ',', true)
		k, v, ok := couper(c, '=', false)
		if !ok || len(k) == 0 || len(v) == 0 {
			return false, nil
		}
		val, garder, ok := valeurChamp(v)
		if !ok {
			return false, nil
		}
		if garder {
			valeurs = append(valeurs, champ{desechapper(k), val})
		}
	}
	if len(valeurs) == 0 {
		return true, nil // seulement des textes : rien à garder, ligne correcte
	}
	sort.Slice(etiquettes, func(i, j int) bool { return etiquettes[i].Nom < etiquettes[j].Nom })
	for i := 1; i < len(etiquettes); i++ {
		if etiquettes[i].Nom == etiquettes[i-1].Nom {
			return false, nil // même tag deux fois (après nettoyage du nom)
		}
	}
	for _, c := range valeurs {
		nom := nomPrometheus(nomMesure+"_"+c.nom, true)
		if nom == "" || len(nom) > d.lim.MaxValeur {
			continue
		}
		var cle strings.Builder
		cle.WriteString(nom)
		for _, e := range etiquettes {
			cle.WriteByte(0)
			cle.WriteString(e.Nom)
			cle.WriteByte(0)
			cle.WriteString(e.Valeur)
		}
		if d.points >= d.lim.MaxPoints {
			return false, fmt.Errorf("%w : plus de %d points", ErrTropGros, d.lim.MaxPoints)
		}
		i, existe := d.index[cle.String()]
		if !existe {
			if len(d.series) >= d.lim.MaxSeries {
				return false, fmt.Errorf("%w : plus de %d séries", ErrTropGros, d.lim.MaxSeries)
			}
			labels := make([]stockage.Label, 0, len(etiquettes)+1)
			labels = append(labels, stockage.Label{Nom: "__name__", Valeur: nom})
			labels = append(labels, etiquettes...)
			sort.Slice(labels, func(a, b int) bool { return labels[a].Nom < labels[b].Nom })
			d.series = append(d.series, stockage.Serie{Labels: labels})
			i = len(d.series) - 1
			d.index[cle.String()] = i
		}
		d.series[i].Points = append(d.series[i].Points, stockage.Point{T: ts, V: c.v})
		d.points++
	}
	return true, nil
}

// valeurChamp : nombre (flottant, entier « 12i », non signé « 12u ») ou
// booléen (1 / 0). Texte entre guillemets : correct mais pas gardé.
func valeurChamp(v []byte) (val float64, garder, ok bool) {
	switch {
	case v[0] == '"':
		return 0, false, len(v) >= 2 && v[len(v)-1] == '"'
	case bytes.Equal(v, []byte("t")), bytes.Equal(v, []byte("T")), bytes.EqualFold(v, []byte("true")):
		return 1, true, true
	case bytes.Equal(v, []byte("f")), bytes.Equal(v, []byte("F")), bytes.EqualFold(v, []byte("false")):
		return 0, true, true
	case v[len(v)-1] == 'i':
		n, err := strconv.ParseInt(string(v[:len(v)-1]), 10, 64)
		return float64(n), err == nil, err == nil
	case v[len(v)-1] == 'u':
		n, err := strconv.ParseUint(string(v[:len(v)-1]), 10, 64)
		return float64(n), err == nil, err == nil
	}
	f, err := strconv.ParseFloat(string(v), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, false
	}
	return f, true, true
}

// couper sépare b au premier séparateur non échappé (« \ » devant). Avec
// guillemets, un séparateur entre guillemets ne compte pas (valeurs texte).
func couper(b []byte, sep byte, guillemets bool) (avant, apres []byte, trouve bool) {
	dans := false
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == '\\':
			i++ // caractère suivant pris tel quel
		case guillemets && c == '"':
			dans = !dans
		case c == sep && !dans:
			return b[:i], b[i+1:], true
		}
	}
	return b, nil, false
}

// desechapper retire les « \ » devant « , », « = », « espace » et « \ ».
func desechapper(b []byte) string {
	if bytes.IndexByte(b, '\\') < 0 {
		return string(b)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && i+1 < len(b) && strings.IndexByte(", =\\\"", b[i+1]) >= 0 {
			i++
		}
		out = append(out, b[i])
	}
	return string(out)
}

// nomPrometheus : caractères non permis remplacés par « _ » (comme
// VictoriaMetrics) ; un nom qui commencerait par un chiffre est précédé de
// « _ ». metrique : « : » permis.
func nomPrometheus(s string, metrique bool) string {
	if s == "" {
		return ""
	}
	b := []byte(s)
	for i, c := range b {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || metrique && c == ':') {
			b[i] = '_'
		}
	}
	if b[0] >= '0' && b[0] <= '9' {
		return "_" + string(b)
	}
	return string(b)
}
