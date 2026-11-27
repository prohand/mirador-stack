// SPDX-License-Identifier: GPL-3.0-or-later

// Package logs traite les lignes de journal avant envoi (SPEC.md § 6.2) :
// filtrage du bruit, limite de débit, masquage des données sensibles,
// compteurs pour les alertes, puis mise en lot pour VictoriaLogs.
package logs

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Remplacement d'une donnée masquée.
const masqueTexte = "***"

// modele : règle de masquage prête à l'emploi.
type modele struct {
	re *regexp.Regexp
	// remplacement : $1 garde le début (le nom du champ), la valeur est masquée.
	remplacement string
	// indices : mots dont l'un doit apparaître (en minuscules) pour que
	// l'expression soit essayée. Évite de passer l'expression sur chaque ligne.
	indices []string
	// verifier : contrôle supplémentaire (numéro de carte : clé de Luhn).
	verifier func(string) bool
}

// Modèles prêts à l'emploi (SPEC.md, « Données sensibles dans les logs »).
// Le moteur regexp de Go est en temps linéaire : pas de risque de blocage.
var modeles = map[string]modele{
	// password=xxx, passwd: xxx, pwd "xxx", mot_de_passe=xxx
	"mots_de_passe": {
		re:           regexp.MustCompile(`(?i)((?:password|passwd|pwd|passphrase|mot_de_passe|mdp)["']?\s*[:=]\s*["']?)[^\s"',;&]+`),
		remplacement: "${1}" + masqueTexte,
		indices:      []string{"pass", "pwd", "mot_de_passe", "mdp"},
	},
	// token=xxx, secret: xxx, access_token=xxx
	"tokens": {
		re:           regexp.MustCompile(`(?i)((?:token|secret|access[_-]?key|session[_-]?id)["']?\s*[:=]\s*["']?)[^\s"',;&]+`),
		remplacement: "${1}" + masqueTexte,
		// Mots exacts de l'expression (relecture perf du 27/11 : « key » et
		// « session » faisaient essayer l'expression sur « publickey » et sur
		// chaque « session opened/closed »).
		indices: []string{"token", "secret", "accesskey", "access_key", "access-key",
			"sessionid", "session_id", "session-id"},
	},
	// api_key=xxx, apikey: xxx, x-api-key: xxx
	"cles_api": {
		re:           regexp.MustCompile(`(?i)((?:api[_-]?key|x-api-key|client[_-]?secret)["']?\s*[:=]\s*["']?)[^\s"',;&]+`),
		remplacement: "${1}" + masqueTexte,
		indices:      []string{"apikey", "api_key", "api-key", "clientsecret", "client_secret", "client-secret"},
	},
	// Authorization: Bearer xxx / Basic xxx (en-têtes HTTP)
	"autorisation": {
		re:           regexp.MustCompile(`(?i)((?:authorization|proxy-authorization)["']?\s*[:=]\s*["']?(?:bearer|basic|token|digest)?\s*)[^\s"',;]+`),
		remplacement: "${1}" + masqueTexte,
		indices:      []string{"authorization"},
	},
	// Numéros de carte : 13 à 19 chiffres (espaces ou tirets possibles),
	// masqués seulement si la clé de Luhn est bonne (moins de faux positifs).
	"cartes": {
		re:       regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`),
		indices:  nil, // essayé seulement s'il y a une suite d'au moins 13 chiffres (voir Masquer)
		verifier: luhn,
	},
	// Adresses e-mail (désactivé par défaut : utile au diagnostic).
	"emails": {
		re:           regexp.MustCompile(`[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,253}\.[A-Za-z]{2,24}`),
		remplacement: masqueTexte,
		indices:      []string{"@"},
	},
}

// ModelesDisponibles renvoie les noms de modèles (pour la validation).
func ModelesDisponibles() []string {
	noms := make([]string, 0, len(modeles))
	for n := range modeles {
		noms = append(noms, n)
	}
	sort.Strings(noms)
	return noms
}

// Limites des expressions personnalisées (coût borné).
const (
	MaxPerso              = 10
	LongueurMaxRe         = 256
	tailleMaxLigneMasquee = 16 << 10
)

// Masqueur applique les règles choisies.
type Masqueur struct {
	regles []modele
	// tampon : copie en minuscules de la ligne, réutilisée d'une ligne à
	// l'autre (évite une allocation par ligne). Masquer n'est appelé que
	// par une goroutine à la fois (sous le verrou du traitement).
	tampon []byte
	// Masquees compte les lignes modifiées (métrique interne).
	Masquees uint64
}

// NewMasqueur crée un masqueur à partir des noms de modèles et des
// expressions personnalisées. Une expression personnalisée masque toute la
// partie qui correspond.
func NewMasqueur(noms []string, perso []string) (*Masqueur, error) {
	m := &Masqueur{}
	for _, n := range noms {
		mod, ok := modeles[n]
		if !ok {
			return nil, fmt.Errorf("modèle de masquage inconnu %q (%s)", n, strings.Join(ModelesDisponibles(), ", "))
		}
		m.regles = append(m.regles, mod)
	}
	if len(perso) > MaxPerso {
		return nil, fmt.Errorf("%d expressions personnalisées au plus", MaxPerso)
	}
	for _, expr := range perso {
		if len(expr) > LongueurMaxRe {
			return nil, fmt.Errorf("expression trop longue (%d caractères max)", LongueurMaxRe)
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("expression invalide %q : %v", expr, err)
		}
		m.regles = append(m.regles, modele{re: re, remplacement: masqueTexte})
	}
	return m, nil
}

// Masquer renvoie la ligne avec les données sensibles remplacées par « *** ».
// Une ligne trop longue n'est masquée que sur ses 16 premiers Kio (le reste
// est coupé à l'envoi, voir Lot).
func (m *Masqueur) Masquer(ligne string) string {
	if len(m.regles) == 0 {
		return ligne
	}
	if len(ligne) > tailleMaxLigneMasquee {
		ligne = ligne[:tailleMaxLigneMasquee]
	}
	var minuscule []byte // calculée seulement si besoin
	avant := ligne
	for i := range m.regles {
		r := &m.regles[i]
		if r.verifier != nil {
			if suiteChiffres(ligne) < 13 {
				continue
			}
		} else if len(r.indices) > 0 {
			if minuscule == nil {
				minuscule = enMinuscules(ligne, &m.tampon)
			}
			trouve := false
			for _, ind := range r.indices {
				if bytes.Contains(minuscule, []byte(ind)) { // conversion sans allocation (chaîne courte)
					trouve = true
					break
				}
			}
			if !trouve {
				continue
			}
		}
		if r.verifier != nil {
			ligne = r.re.ReplaceAllStringFunc(ligne, func(s string) string {
				if r.verifier(s) {
					return masqueTexte
				}
				return s
			})
		} else {
			ligne = r.re.ReplaceAllString(ligne, r.remplacement)
		}
		minuscule = nil
	}
	if ligne != avant {
		m.Masquees++
	}
	return ligne
}

// suiteChiffres : plus longue suite de chiffres séparés au plus par UNE
// espace ou UN tiret (ce que demande l'expression des cartes). Sous 13,
// l'expression ne peut pas trouver de carte : elle n'est pas essayée.
// Relecture perf du 27/11 : compter TOUS les chiffres de la ligne faisait
// passer l'expression sur presque chaque ligne (adresses IP, heures, PID).
func suiteChiffres(s string) int {
	plus, n, sep := 0, 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			n++
			sep = false
			plus = max(plus, n)
		case (c == ' ' || c == '-') && n > 0 && !sep:
			sep = true // un séparateur entre deux chiffres
		default:
			n, sep = 0, false
		}
	}
	return plus
}

// enMinuscules : s en minuscules, écrit dans *tampon (réutilisé : pas
// d'allocation par ligne). Le résultat n'est valable que jusqu'au prochain
// appel. Une ligne non ASCII passe par strings.ToLower (règles Unicode,
// ex. signe Kelvin → k, comme l'expression en (?i)).
func enMinuscules(s string, tampon *[]byte) []byte {
	b := (*tampon)[:0]
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			b = append(b[:0], strings.ToLower(s)...)
			break
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	*tampon = b
	return b
}

// luhn vérifie la clé d'un numéro de carte (espaces et tirets ignorés).
func luhn(s string) bool {
	somme, n, double := 0, 0, false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c == ' ' || c == '-' {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		somme += d
		double = !double
		n++
	}
	return n >= 13 && n <= 19 && somme%10 == 0
}
