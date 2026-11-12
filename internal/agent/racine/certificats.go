// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"mirador-stack/internal/agent/config"
)

// Certificats : résultat du parcours des certificats locaux (remplace
// obs-certs.sh, SPEC.md § 4.2). Lecture en Go (crypto/x509), sans openssl.
type Certificats struct {
	Liste   []Certificat `json:"liste,omitempty"`
	Trouves int          `json:"trouves"` // ssl_certs_found_total
	Erreurs int          `json:"erreurs"` // ssl_certs_read_errors_total
	// Motifs : compte rendu de chaque chemin ajouté dans l'interface (12/11),
	// pour dire « aucun fichier », « 2 certificats lus », « écarté »…
	Motifs []ResultatMotif `json:"motifs,omitempty"`
}

// ResultatMotif : ce qu'a donné un chemin venu du serveur. Les fichiers
// écartés le sont pour une raison précise, affichée dans l'interface.
type ResultatMotif struct {
	Motif    string `json:"motif"`
	Fichiers int    `json:"fichiers"` // fichiers trouvés par le motif
	Lus      int    `json:"lus"`      // certificats lus
	Ecartes  int    `json:"ecartes"`  // total des fichiers écartés (raisons ci-dessous)
	// Raisons : nom de clé privée ; lien (ou fichier) hors des dossiers
	// autorisés ; pas un certificat lisible (autre format, trop gros, tube…) ;
	// même fichier déjà lu par un autre chemin.
	Cles        int `json:"cles,omitempty"`
	HorsDossier int `json:"hors_dossier,omitempty"`
	Illisibles  int `json:"illisibles,omitempty"`
	Doubles     int `json:"doubles,omitempty"`
}

// Certificat : un certificat lu avec succès.
type Certificat struct {
	Chemin   string    `json:"chemin"`
	CN       string    `json:"cn"`
	Emetteur string    `json:"emetteur"`
	Debut    time.Time `json:"debut"`
	Fin      time.Time `json:"fin"`
}

// Limites de protection.
const (
	maxCertificats    = 500
	tailleMaxCertFich = 1 << 20
	// Fichiers examinés pour un motif venu du serveur : un joker trop large
	// ne fait pas ouvrir des milliers de fichiers par root.
	maxFichiersDistant = 100
)

// lireCertificats parcourt les motifs du fichier local (ex.
// /etc/letsencrypt/live/*/cert.pem), puis ceux ajoutés dans l'interface
// (distants), avec des contrôles en plus pour ces derniers (SPEC.md § 21.3).
// Un même fichier atteint par deux chemins (lien symbolique, cas de
// /etc/pve/local) n'est compté qu'une fois (dédoublonnage par inode).
func lireCertificats(locaux, distants []string) *Certificats {
	c := &Certificats{}
	vus := map[uint64]bool{}
	parcourir := func(motif string, distant bool) {
		res := ResultatMotif{Motif: motif}
		if distant {
			defer func() { c.Motifs = append(c.Motifs, res) }()
		}
		fichiers, err := filepath.Glob(motif)
		if err != nil {
			return // motif validé à la lecture de la configuration
		}
		sort.Strings(fichiers)
		res.Fichiers = len(fichiers)
		if distant && len(fichiers) > maxFichiersDistant {
			fichiers = fichiers[:maxFichiersDistant]
			c.Erreurs++
		}
		for _, f := range fichiers {
			if len(c.Liste) >= maxCertificats {
				return
			}
			if distant {
				switch raisonDistant(f) {
				case "cle":
					res.Ecartes++
					res.Cles++
					continue
				case "hors_dossier":
					res.Ecartes++
					res.HorsDossier++
					continue
				}
			}
			cert, ino, ok := lireCertificat(f)
			if ino != 0 {
				if vus[ino] {
					res.Ecartes++
					res.Doubles++
					continue
				}
				vus[ino] = true
			}
			if !ok {
				// Clé privée, fichier non reconnu ou illisible dans un dossier scanné.
				c.Erreurs++
				res.Ecartes++
				res.Illisibles++
				continue
			}
			c.Liste = append(c.Liste, cert)
			c.Trouves++
			res.Lus++
		}
	}
	for _, m := range locaux {
		parcourir(m, false)
	}
	for _, m := range distants {
		parcourir(m, true)
	}
	return c
}

// dossierAutorise : remplacé dans les tests (les fichiers de test ne sont
// pas sous /etc).
var dossierAutorise = config.DossierDistantAutorise

// fichierDistantSur : pour un fichier trouvé par un motif venu du serveur, le
// nom ne doit pas ressembler à une clé privée, et le fichier réel (liens
// suivis) doit rester dans un dossier autorisé, sous un nom autorisé.
func fichierDistantSur(f string) bool { return raisonDistant(f) == "" }

// raisonDistant : ” si le fichier peut être lu, sinon la raison du refus
// (« cle » : nom de clé privée ; « hors_dossier » : lien dont la cible sort
// des dossiers autorisés, ou lien cassé).
func raisonDistant(f string) string {
	if !config.FichierAccepteDistant(f) {
		return "cle"
	}
	reel, err := filepath.EvalSymlinks(f)
	if err != nil {
		return "hors_dossier"
	}
	if !config.FichierAccepteDistant(reel) {
		return "cle"
	}
	if !dossierAutorise(reel) {
		return "hors_dossier"
	}
	return ""
}

// lireCertificat lit le premier certificat PEM du fichier (le certificat du
// serveur dans un fullchain.pem, comme « openssl x509 »). Le fichier est
// ouvert sans bloquer puis vérifié (fichier ordinaire, taille bornée) : un
// tube ou un périphérique ne peut pas bloquer l'assistant. Renvoie aussi
// l'inode (0 si inconnu) pour le dédoublonnage.
func lireCertificat(chemin string) (Certificat, uint64, bool) {
	f, err := os.OpenFile(chemin, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0) // #nosec G304 -- fichiers trouvés par les motifs validés
	if err != nil {
		return Certificat{}, 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Certificat{}, 0, false
	}
	var ino uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}
	if !info.Mode().IsRegular() || info.Size() > tailleMaxCertFich {
		return Certificat{}, ino, false
	}
	brut, err := io.ReadAll(io.LimitReader(f, tailleMaxCertFich))
	if err != nil {
		return Certificat{}, ino, false
	}
	for {
		var bloc *pem.Block
		bloc, brut = pem.Decode(brut)
		if bloc == nil {
			return Certificat{}, ino, false
		}
		if bloc.Type != "CERTIFICATE" {
			continue
		}
		x, err := x509.ParseCertificate(bloc.Bytes)
		if err != nil {
			return Certificat{}, ino, false
		}
		cn := x.Subject.CommonName
		if cn == "" {
			cn = "(sans CN)"
		}
		emetteur := x.Issuer.CommonName
		if emetteur == "" {
			emetteur = "(inconnu)"
		}
		return Certificat{Chemin: chemin, CN: cn, Emetteur: emetteur,
			Debut: x.NotBefore.UTC(), Fin: x.NotAfter.UTC()}, ino, true
	}
}

// tailleMaxConfigDistante : le fichier gardé par l'agent ne dépasse jamais
// 256 Kio (taille maximale d'un document accepté).
const tailleMaxConfigDistante = 256 << 10

// MotifsDistants lit SEULEMENT la clé « certificats_ajout » de la dernière
// configuration reçue du serveur, gardée par l'agent (SPEC.md § 21.3).
//
// Ce fichier appartient à l'agent (sans root) : root le lit donc comme une
// donnée non fiable. Pas de lien symbolique suivi, fichier ordinaire et petit,
// message d'erreur sans contenu du fichier, et chaque motif est revérifié
// avec les règles strictes (ValiderMotifDistant). Un motif refusé est sauté
// et compté ; les autres restent valables.
func MotifsDistants(chemin string) (motifs []string, refuses int, err error) {
	f, err := os.OpenFile(chemin, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0) // #nosec G304 -- chemin de la configuration locale
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil // pas encore de configuration distante
		}
		return nil, 0, errors.New("configuration distante gardée : ouverture refusée (lien symbolique ?)")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > tailleMaxConfigDistante {
		return nil, 0, errors.New("configuration distante gardée : pas un petit fichier ordinaire")
	}
	var doc struct {
		CertificatsAjout []string `json:"certificats_ajout"`
	}
	if json.NewDecoder(io.LimitReader(f, tailleMaxConfigDistante)).Decode(&doc) != nil {
		return nil, 0, errors.New("configuration distante gardée illisible")
	}
	for i, m := range doc.CertificatsAjout {
		if i >= config.MaxMotifsDistants || config.ValiderMotifDistant(m) != nil {
			refuses++
			continue
		}
		motifs = append(motifs, m)
	}
	if refuses > 0 {
		err = fmt.Errorf("%d chemin(s) de certificat venus du serveur refusés", refuses)
	}
	return motifs, refuses, err
}
