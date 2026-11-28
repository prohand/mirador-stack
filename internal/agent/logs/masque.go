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

// Morceaux communs aux modèles « champ = valeur » (revue du 28/11).
const (
	// guillemetCle : guillemet facultatif après le nom du champ, échappé ou
	// non (« "password": », ou « \"password\": » dans du JSON échappé).
	guillemetCle = `(?:\\?["'])?`
	// valeurCle : la valeur à masquer. Le guillemet ouvrant est capturé
	// (groupes 2 à 4) pour être remis devant « *** ». Une valeur entre
	// guillemets est masquée en entier, espaces compris :
	//   "valeur avec espaces"   'valeur'   \"valeur en JSON échappé\"
	// Sinon : jusqu'à une espace, un guillemet, « , ; & » ou « \" ».
	valeurCle = `(?:(")[^"\n]+|(')[^'\n]+|(\\")(?:[^"\\\n]|\\[^"\n])+|(?:[^\s"',;&\\]|\\[^\s"'])+)`
	// rempCle : remet le nom du champ et le guillemet ouvrant éventuel.
	rempCle = "${1}${2}${3}${4}" + masqueTexte
)

// regleCle : règle « champ = valeur » pour une liste de noms de champs.
func regleCle(noms string, indices ...string) modele {
	return modele{
		re:           regexp.MustCompile(`(?i)((?:` + noms + `)` + guillemetCle + `\s*[:=]\s*)` + valeurCle),
		remplacement: rempCle,
		indices:      indices,
	}
}

// Modèles prêts à l'emploi (SPEC.md, « Données sensibles dans les logs »).
// Un modèle peut compter plusieurs règles, chacune avec ses indices.
// Le moteur regexp de Go est en temps linéaire : pas de risque de blocage.
var modeles = map[string][]modele{
	"mots_de_passe": {
		// password=xxx, passwd: xxx, pwd "xxx", mot_de_passe=xxx,
		// {"password":"a b"}, {\"password\":\"x\"}
		regleCle(`password|passwd|pwd|passphrase|mot_de_passe|mdp`, "pass", "pwd", "mot_de_passe", "mdp"),
		// Option de ligne de commande : --password secret, --pass=secret.
		// La valeur ne commence pas par « - » (ce serait l'option suivante).
		{
			re:           regexp.MustCompile(`(?i)((?:^|[\s"'(])--?(?:password|passwd|pass|pwd)(?:\s+|=))(?:(")[^"\n]+|(')[^'\n]+|[^\s"'-][^\s"']*)`),
			remplacement: rempCle,
			indices:      []string{"-pass", "-pwd"},
		},
		// mysql / mariadb : -pSECRET (collé à -p). Limité à ces programmes :
		// « -p » veut dire autre chose ailleurs (ssh -p 22, mkdir -p).
		// « -p » suivi d'une espace (le mot de passe est demandé) est laissé.
		{
			re:           regexp.MustCompile(`((?i:mysql[a-z]*|mariadb[a-z-]*)\s(?:[^\n]*?\s)?-p)[^\s"']+`),
			remplacement: "${1}" + masqueTexte,
			indices:      []string{"mysql", "mariadb"},
		},
		// Identifiants dans une adresse : postgres://app:secret@hote → le mot
		// de passe seulement (le nom d'utilisateur aide au diagnostic).
		{
			re:           regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s:/@"'<>]*:)[^\s/"'<>]+@`),
			remplacement: "${1}" + masqueTexte + "@",
			indices:      []string{"://"},
		},
	},
	// token=xxx, secret: xxx, access_token=xxx
	"tokens": {
		// Mots exacts de l'expression (relecture perf du 27/11 : « key » et
		// « session » faisaient essayer l'expression sur « publickey » et sur
		// chaque « session opened/closed »).
		regleCle(`token|secret|access[_-]?key|session[_-]?id`, "token", "secret", "accesskey", "access_key", "access-key",
			"sessionid", "session_id", "session-id"),
	},
	// api_key=xxx, apikey: xxx, x-api-key: xxx
	"cles_api": {
		regleCle(`api[_-]?key|x-api-key|client[_-]?secret`, "apikey", "api_key", "api-key", "clientsecret", "client_secret", "client-secret"),
	},
	"autorisation": {
		// Authorization: Bearer xxx / Basic xxx (en-têtes HTTP), aussi en
		// JSON échappé (\"authorization\":\"Bearer xxx\").
		{
			re:           regexp.MustCompile(`(?i)((?:authorization|proxy-authorization)` + guillemetCle + `\s*[:=]\s*(?:\\?["'])?(?:bearer|basic|token|digest)?\s*)[^\s"',;\\]+`),
			remplacement: "${1}" + masqueTexte,
			indices:      []string{"authorization"},
		},
		// En-têtes Cookie: / Set-Cookie: : toute la valeur (plusieurs
		// cookies séparés par « ; »), jusqu'au guillemet fermant ou à la fin.
		{
			re:           regexp.MustCompile(`(?i)\b((?:set-)?cookie` + guillemetCle + `\s*:\s*)(?:(")[^"\n]+|(')[^'\n]+|(\\")(?:[^"\\\n]|\\[^"\n])+|[^\s"'\\][^"'\\\n]*)`),
			remplacement: rempCle,
			indices:      []string{"cookie"},
		},
	},
	// Numéros de carte : 13 à 19 chiffres (espaces ou tirets possibles),
	// masqués seulement si la clé de Luhn est bonne (moins de faux positifs).
	"cartes": {{
		re:       regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`),
		indices:  nil, // essayé seulement s'il y a une suite d'au moins 13 chiffres (voir Masquer)
		verifier: luhn,
	}},
	// Adresses e-mail (désactivé par défaut : utile au diagnostic).
	"emails": {{
		re:           regexp.MustCompile(`[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,253}\.[A-Za-z]{2,24}`),
		remplacement: masqueTexte,
		indices:      []string{"@"},
	}},
}

// ModelesPlancher : modèles de secrets (mots de passe, jetons, clés d'API,
// en-têtes d'autorisation et cookies) toujours actifs sur l'agent. Le
// serveur peut ajouter des modèles, jamais retirer ceux-ci ; seul le réglage
// local MIRADOR_MASQUAGE_PLANCHER=non les rend facultatifs (revue du 28/11).
var ModelesPlancher = []string{"mots_de_passe", "tokens", "cles_api", "autorisation"}

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
	// paires : empreinte de la ligne en minuscules (voir empreinte).
	paires empreinte
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
		m.regles = append(m.regles, mod...)
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
				m.paires.remplir(minuscule)
			}
			trouve := false
			for _, ind := range r.indices {
				// Les paires d'abord (quelques opérations), bytes.Contains
				// seulement si toutes les paires de l'indice sont présentes.
				if m.paires.peutContenir(ind) && bytes.Contains(minuscule, []byte(ind)) { // conversion sans allocation (chaîne courte)
					trouve = true
					break
				}
			}
			if !trouve {
				continue
			}
		}
		if nouvelle, change := r.remplacer(ligne); change {
			ligne = nouvelle
			minuscule = nil // à recalculer pour les règles suivantes
		}
	}
	if ligne != avant {
		m.Masquees++
	}
	return ligne
}

// remplacer masque ce que la règle trouve dans la ligne. L'expression n'est
// passée qu'une fois ; sans résultat, la ligne n'est pas copiée
// (ReplaceAllString copie toujours, et « Failed password for » trouve
// l'indice sans rien à masquer ; relecture perf du 28/11).
func (r *modele) remplacer(ligne string) (string, bool) {
	trouves := r.re.FindAllStringSubmatchIndex(ligne, -1)
	if trouves == nil {
		return ligne, false
	}
	out := make([]byte, 0, len(ligne)+8)
	fin := 0
	for _, t := range trouves {
		out = append(out, ligne[fin:t[0]]...)
		switch {
		case r.verifier == nil:
			out = r.re.ExpandString(out, r.remplacement, ligne, t)
		case r.verifier(ligne[t[0]:t[1]]):
			out = append(out, masqueTexte...)
		default: // pas une vraie carte : gardée telle quelle
			out = append(out, ligne[t[0]:t[1]]...)
		}
		fin = t[1]
	}
	out = append(out, ligne[fin:]...)
	return string(out), true
}

// empreinte : paires d'octets voisins présentes dans la ligne, rangées dans
// 1024 bits. Si une paire d'un indice manque, l'indice n'est pas dans la
// ligne et bytes.Contains est évité. Deux paires peuvent tomber sur le même
// bit : l'empreinte peut dire « peut-être » à tort, jamais « non » à tort.
// Relecture perf du 28/11 : la recherche des indices prenait 60 % du temps.
type empreinte [16]uint64

// bitPaire : numéro de bit (0 à 1023) de la paire (a, b).
func bitPaire(a, b byte) uint { return (uint(a)<<5 ^ uint(b)) & 1023 }

// remplir calcule l'empreinte de b.
func (e *empreinte) remplir(b []byte) {
	*e = empreinte{}
	for i := 1; i < len(b); i++ {
		n := bitPaire(b[i-1], b[i])
		e[n>>6] |= 1 << (n & 63)
	}
}

// peutContenir : faux si ind ne peut pas être dans la ligne.
func (e *empreinte) peutContenir(ind string) bool {
	for i := 1; i < len(ind); i++ {
		n := bitPaire(ind[i-1], ind[i])
		if e[n>>6]&(1<<(n&63)) == 0 {
			return false
		}
	}
	return true
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
