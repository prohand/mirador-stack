// SPDX-License-Identifier: GPL-3.0-or-later

// Package collect contient les collecteurs système de mirador-agent.
//
// Chaque collecteur lit directement /proc ou /sys (aucune commande externe) et
// ajoute ses points au lot du cycle. Les noms et étiquettes sont ceux de
// node_exporter, pour garder les dashboards et règles actuels (SPEC.md § 4.1).
//
// Pour limiter les allocations, les fichiers sont lus dans un tampon réutilisé
// et analysés en []byte, sans découpage en chaînes.
package collect

import (
	"bytes"
	"errors"
	"io"
	"os"

	"mirador-stack/internal/agent/metrics"
)

// Collecteur est implémenté par chaque collecteur.
type Collecteur interface {
	// Nom court, utilisé dans les journaux et les métriques internes.
	Nom() string
	// Lent : vrai si le collecteur tourne au rythme lent (ex. 60 s).
	Lent() bool
	// Collecter ajoute les points du cycle au lot.
	Collecter(b *metrics.Batch) error
}

// lecteur lit un fichier entier dans un tampon réutilisé d'un appel à l'autre.
type lecteur struct {
	buf []byte
}

// tailleMaxFichier borne la lecture : un fichier de /proc plus gros que ça
// serait anormal (protection mémoire).
const tailleMaxFichier = 4 << 20

// lire renvoie le contenu du fichier. Le résultat n'est valable que jusqu'au
// prochain appel (le tampon est réutilisé).
func (l *lecteur) lire(chemin string) ([]byte, error) {
	f, err := os.Open(chemin) // #nosec G304 -- chemins construits par l'agent sous /proc et /sys
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if cap(l.buf) == 0 {
		l.buf = make([]byte, 0, 16<<10)
	}
	l.buf = l.buf[:0]
	for {
		if len(l.buf) == cap(l.buf) {
			if cap(l.buf) >= tailleMaxFichier {
				return nil, errors.New(chemin + " : fichier trop gros")
			}
			// Agrandit le tampon (rare : seulement la première fois).
			l.buf = append(l.buf, 0)[:len(l.buf)]
		}
		n, err := f.Read(l.buf[len(l.buf):cap(l.buf)])
		l.buf = l.buf[:len(l.buf)+n]
		if err == io.EOF {
			return l.buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// --- Petits outils d'analyse sans allocation ---------------------------------

// ligneSuivante découpe la première ligne de b. Renvoie la ligne et le reste.
func ligneSuivante(b []byte) (ligne, reste []byte) {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i], b[i+1:]
	}
	return b, nil
}

// champSuivant renvoie le premier mot (séparé par espaces ou tabulations).
func champSuivant(b []byte) (champ, reste []byte) {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
		i++
	}
	j := i
	for j < len(b) && b[j] != ' ' && b[j] != '\t' {
		j++
	}
	return b[i:j], b[j:]
}

// entier lit un entier positif en décimal. ok est faux si b n'en est pas un.
func entier(b []byte) (v uint64, ok bool) {
	if len(b) == 0 || len(b) > 20 {
		return 0, false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return v, true
}

// decimal lit un nombre de la forme 123 ou 123.456 (format de /proc/loadavg).
func decimal(b []byte) (float64, bool) {
	ent, frac, aVirgule := b, []byte(nil), false
	if i := bytes.IndexByte(b, '.'); i >= 0 {
		ent, frac, aVirgule = b[:i], b[i+1:], true
	}
	e, ok := entier(ent)
	if !ok {
		return 0, false
	}
	v := float64(e)
	if aVirgule {
		f, ok := entier(frac)
		if !ok {
			return 0, false
		}
		div := 1.0
		for range frac {
			div *= 10
		}
		v += float64(f) / div
	}
	return v, true
}

// apresPrefixe renvoie ce qui suit prefixe dans b, ou nil.
func apresPrefixe(b []byte, prefixe string) []byte {
	if len(b) >= len(prefixe) && string(b[:len(prefixe)]) == prefixe {
		return b[len(prefixe):]
	}
	return nil
}
