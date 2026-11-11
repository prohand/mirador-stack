// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Certificats : résultat du parcours des certificats locaux (remplace
// obs-certs.sh, SPEC.md § 4.2). Lecture en Go (crypto/x509), sans openssl.
type Certificats struct {
	Liste   []Certificat `json:"liste,omitempty"`
	Trouves int          `json:"trouves"` // ssl_certs_found_total
	Erreurs int          `json:"erreurs"` // ssl_certs_read_errors_total
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
)

// lireCertificats parcourt les motifs (ex. /etc/letsencrypt/live/*/cert.pem).
// Un même fichier atteint par deux chemins (lien symbolique, cas de
// /etc/pve/local) n'est compté qu'une fois (dédoublonnage par inode).
func lireCertificats(motifs []string, _ time.Time) *Certificats {
	c := &Certificats{}
	vus := map[uint64]bool{}
	for _, motif := range motifs {
		fichiers, err := filepath.Glob(motif)
		if err != nil {
			continue // motif validé à la lecture de la configuration
		}
		sort.Strings(fichiers)
		for _, f := range fichiers {
			if len(c.Liste) >= maxCertificats {
				return c
			}
			info, err := os.Stat(f)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				if vus[st.Ino] {
					continue
				}
				vus[st.Ino] = true
			}
			cert, ok := lireCertificat(f, info.Size())
			if !ok {
				// Clé privée ou fichier non reconnu dans un dossier scanné.
				c.Erreurs++
				continue
			}
			c.Liste = append(c.Liste, cert)
			c.Trouves++
		}
	}
	return c
}

// lireCertificat lit le premier certificat PEM du fichier (le certificat du
// serveur dans un fullchain.pem, comme « openssl x509 »).
func lireCertificat(chemin string, taille int64) (Certificat, bool) {
	if taille > tailleMaxCertFich {
		return Certificat{}, false
	}
	brut, err := os.ReadFile(chemin) // #nosec G304 -- fichiers trouvés par les motifs de la configuration
	if err != nil {
		return Certificat{}, false
	}
	for {
		var bloc *pem.Block
		bloc, brut = pem.Decode(brut)
		if bloc == nil {
			return Certificat{}, false
		}
		if bloc.Type != "CERTIFICATE" {
			continue
		}
		x, err := x509.ParseCertificate(bloc.Bytes)
		if err != nil {
			return Certificat{}, false
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
			Debut: x.NotBefore.UTC(), Fin: x.NotAfter.UTC()}, true
	}
}
