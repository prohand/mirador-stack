// SPDX-License-Identifier: GPL-3.0-or-later

// Package majagent : mise à jour signée de mirador-agent (SPEC.md § 6.8).
//
// DÉSACTIVÉE tant que le serveur n'existe pas (et toujours sans l'option
// --auto-update à l'installation). Le code est prêt et testé.
//
// Règles de sécurité :
//   - aucun binaire n'est installé sans signature Ed25519 valide d'une des
//     clés publiques posées à l'installation (fichier root) ou intégrées au
//     binaire (la clé privée reste chez l'utilisateur, jamais sur le serveur
//     ni dans le dépôt) ;
//   - empreinte SHA-256 et taille vérifiées ;
//   - jamais de version plus ancienne, sauf instruction signée explicite ;
//   - le nouveau binaire passe son -self-test avant d'être activé ;
//   - remplacement atomique, ancienne version conservée, retour arrière possible.
package majagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	clesdepot "mirador-stack/cles"
	"mirador-stack/internal/protocole"
)

// ClesPubliques : clés publiques Ed25519 (base64, séparées par des virgules)
// intégrées à la compilation depuis cles/publiques.txt (go:embed),
// FACULTATIVES : les clés sont normalement posées à l'installation dans un
// fichier root (voir ClesInstallees, SPEC § 18.25). Modifiable seulement
// dans les tests.
var ClesPubliques = clesdepot.Publiques()

// TailleMax d'un binaire accepté (protection disque et mémoire).
const TailleMax = 64 << 20

// Erreurs de vérification.
var (
	ErrSansCle       = errors.New("aucune clé publique intégrée : mises à jour impossibles")
	ErrSignature     = errors.New("signature invalide")
	ErrEmpreinte     = errors.New("empreinte SHA-256 ou taille différente")
	ErrRetourArriere = errors.New("version plus ancienne refusée (retour arrière non signé)")
	ErrArchitecture  = errors.New("architecture différente")
)

// cles décode les clés publiques intégrées.
func cles(brut string) ([]ed25519.PublicKey, error) {
	var out []ed25519.PublicKey
	for _, c := range strings.Split(brut, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		k, err := base64.StdEncoding.DecodeString(c)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("clé publique intégrée invalide %q", c)
		}
		out = append(out, ed25519.PublicKey(k))
	}
	if len(out) == 0 {
		return nil, ErrSansCle
	}
	return out, nil
}

// VerifierManifeste contrôle la signature et la cohérence du manifeste, AVANT
// tout téléchargement du binaire.
func VerifierManifeste(m *protocole.Manifeste, versionActuelle, clesIntegrees string) error {
	if err := VerifierSignature(m, clesIntegrees); err != nil {
		return err
	}
	if m.Architecture != runtime.GOARCH {
		return ErrArchitecture
	}
	cmp, err := ComparerVersions(m.Version, versionActuelle)
	if err != nil {
		return err
	}
	if cmp == 0 {
		return errors.New("déjà à cette version")
	}
	if cmp < 0 && !m.RetourAutorise {
		return ErrRetourArriere
	}
	return nil
}

// VerifierSignature contrôle seulement la signature et la forme du manifeste
// (taille, empreinte), sans regarder l'architecture ni la version : sert au
// serveur, qui publie des versions pour toutes les architectures.
func VerifierSignature(m *protocole.Manifeste, clesIntegrees string) error {
	pub, err := cles(clesIntegrees)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return ErrSignature
	}
	signe := false
	for _, k := range pub {
		if ed25519.Verify(k, m.MessageSigne(), sig) {
			signe = true
			break
		}
	}
	if !signe {
		return ErrSignature
	}
	if m.Taille <= 0 || m.Taille > TailleMax {
		return fmt.Errorf("taille annoncée invalide : %d", m.Taille)
	}
	if _, err := hex.DecodeString(m.SHA256); err != nil || len(m.SHA256) != 64 {
		return errors.New("empreinte annoncée invalide")
	}
	return nil
}

// VerifierBinaire contrôle le binaire téléchargé contre le manifeste signé.
func VerifierBinaire(m *protocole.Manifeste, binaire []byte) error {
	if int64(len(binaire)) != m.Taille {
		return ErrEmpreinte
	}
	somme := sha256.Sum256(binaire)
	attendu, _ := hex.DecodeString(m.SHA256)
	if !bytes.Equal(somme[:], attendu) {
		return ErrEmpreinte
	}
	return nil
}

// ComparerVersions compare deux versions « 1.2.3 » : -1, 0 ou 1.
// Une version « dev » (compilation locale) n'est jamais comparable.
func ComparerVersions(a, b string) (int, error) {
	pa, err := analyserVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := analyserVersion(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < 3; i++ {
		switch {
		case pa[i] < pb[i]:
			return -1, nil
		case pa[i] > pb[i]:
			return 1, nil
		}
	}
	return 0, nil
}

func analyserVersion(v string) ([3]int, error) {
	var out [3]int
	parties := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parties) != 3 {
		return out, fmt.Errorf("version %q : forme 1.2.3 attendue", v)
	}
	for i, p := range parties {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 99999 {
			return out, fmt.Errorf("version %q : forme 1.2.3 attendue", v)
		}
		out[i] = n
	}
	return out, nil
}
