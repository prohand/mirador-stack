// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Mots de passe : Argon2id (brief), paramètres minimaux conseillés par
// l'OWASP (19 Mio, 2 passes, 1 fil). Une empreinte coûte ~19 Mio de RAM
// pendant ~30 ms : au plus 2 calculs en même temps (SPEC § 18.2), soit
// 38 Mio au pire, même sous une avalanche de tentatives.
const (
	argonTemps   = 2
	argonMemoire = 19 * 1024 // Kio
	argonFils    = 1
	argonTaille  = 32

	MotDePasseMin = 12
	MotDePasseMax = 128
)

// calculs limite les calculs Argon2id simultanés.
var calculs = make(chan struct{}, 2)

// attenteCalcul : au-delà, « serveur occupé » plutôt qu'une file sans fin
// (avalanche de connexions : les vraies ne restent pas bloquées, revue du
// 11/11).
const attenteCalcul = 10 * time.Second

// ErrOccupe : trop de calculs Argon2id en attente.
var ErrOccupe = errors.New("serveur occupé : réessayer dans un instant")

func argon(mdp string, sel []byte, t, m uint32, p uint8) ([]byte, error) {
	minuteur := time.NewTimer(attenteCalcul)
	defer minuteur.Stop()
	select {
	case calculs <- struct{}{}:
	case <-minuteur.C:
		return nil, ErrOccupe
	}
	defer func() { <-calculs }()
	return argon2.IDKey([]byte(mdp), sel, t, m, p, argonTaille), nil
}

// VerifierRegles : longueur seulement (12 à 128 caractères). Pas de règle
// de composition : une longue phrase vaut mieux (recommandation ANSSI/NIST).
func VerifierRegles(mdp string) error {
	n := utf8.RuneCountInString(mdp)
	if n < MotDePasseMin || n > MotDePasseMax {
		return fmt.Errorf("mot de passe : entre %d et %d caractères", MotDePasseMin, MotDePasseMax)
	}
	return nil
}

// VerifierSimple refuse les mots de passe assez longs mais devinés en
// premier : un seul caractère répété, une suite (« 123456789012 »,
// « abcdefghijkl »), un motif court répété (« abcabcabcabc »), ou le nom du
// compte / « mirador » complété par quelques caractères. Pas de liste de
// mots de passe connus à embarquer : la longueur (12) et la 2FA font le reste.
func VerifierSimple(mdp, nom string) error {
	trop := errors.New("mot de passe trop simple (répétition, suite, ou nom du compte)")
	r := []rune(strings.ToLower(mdp))
	if len(r) == 0 {
		return nil // la longueur est vérifiée ailleurs
	}
	// Suite : chaque caractère suit le précédent (+1, −1) ou le répète (0).
	suite := true
	pas := func(a, b rune) rune { // « 9 » puis « 0 » compte comme une suite
		if a == '9' && b == '0' {
			return 1
		}
		return b - a
	}
	for i := 1; i < len(r) && suite; i++ {
		d := pas(r[i-1], r[i])
		suite = d == pas(r[0], r[1]) && d >= -1 && d <= 1
	}
	if suite {
		return trop
	}
	// Motif de 1 à 4 caractères répété sur toute la longueur.
	for n := 1; n <= 4 && n < len(r); n++ {
		repete := true
		for i := n; i < len(r) && repete; i++ {
			repete = r[i] == r[i-n]
		}
		if repete {
			return trop
		}
	}
	// Nom du compte ou « mirador » : sans lui, il doit rester 8 caractères
	// qui ne sont pas eux-mêmes une suite ou une répétition.
	bas := string(r)
	for _, mot := range []string{strings.ToLower(nom), "mirador"} {
		if mot == "" || !strings.Contains(bas, mot) {
			continue
		}
		reste := strings.Trim(strings.Replace(bas, mot, "", 1), " -_.,;:!?/+*@#")
		if utf8.RuneCountInString(reste) < 8 || VerifierSimple(reste, "") != nil {
			return trop
		}
	}
	return nil
}

// Empreinte renvoie l'empreinte à garder en base (format PHC :
// $argon2id$v=19$m=…,t=…,p=…$sel$empreinte).
func Empreinte(mdp string) (string, error) {
	if err := VerifierRegles(mdp); err != nil {
		return "", err
	}
	sel := make([]byte, 16)
	if _, err := rand.Read(sel); err != nil {
		return "", err
	}
	e, err := argon(mdp, sel, argonTemps, argonMemoire, argonFils)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemoire, argonTemps, argonFils,
		base64.RawStdEncoding.EncodeToString(sel), base64.RawStdEncoding.EncodeToString(e)), nil
}

// EmpreinteLeurre : comparée quand le compte n'existe pas, pour que la
// durée de réponse ne dise pas si un nom d'utilisateur existe.
var EmpreinteLeurre, _ = Empreinte("leurre-pour-un-compte-inconnu")

// Comparer dit si mdp correspond à l'empreinte (temps constant).
func Comparer(empreinte, mdp string) bool {
	ok, _ := ComparerOccupe(empreinte, mdp)
	return ok
}

// ComparerOccupe : comme Comparer ; occupe = vrai si le calcul n'a pas pu
// se faire (ErrOccupe) : ce n'est PAS un échec à compter.
func ComparerOccupe(empreinte, mdp string) (ok, occupe bool) {
	if len(mdp) > 4*MotDePasseMax {
		return false, false
	}
	p := strings.Split(empreinte, "$")
	if len(p) != 6 || p[1] != "argon2id" || p[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, false
	}
	var m, t uint32
	var f uint8
	if _, err := fmt.Sscanf(p[3], "m=%d,t=%d,p=%d", &m, &t, &f); err != nil {
		return false, false
	}
	// Bornes : une empreinte abîmée ne doit pas faire exploser la mémoire.
	if m < 8*1024 || m > 256*1024 || t < 1 || t > 10 || f < 1 || f > 4 {
		return false, false
	}
	sel, err1 := base64.RawStdEncoding.DecodeString(p[4])
	attendu, err2 := base64.RawStdEncoding.DecodeString(p[5])
	if err := errors.Join(err1, err2); err != nil || len(attendu) != argonTaille {
		return false, false
	}
	calcule, err := argon(mdp, sel, t, m, f)
	if err != nil {
		return false, true
	}
	return subtle.ConstantTimeCompare(calcule, attendu) == 1, false
}
