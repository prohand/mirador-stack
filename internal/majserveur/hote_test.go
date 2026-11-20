// SPDX-License-Identifier: GPL-3.0-or-later

package majserveur

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/release"
)

// dockerSimule : conteneur simulé. « compose up -d » démarre la version du
// .env : si elle est dans demarre, le « serveur » écrit sa présence.
type dockerSimule struct {
	t          *testing.T
	r          Reglages
	maintenant func() time.Time
	demarre    map[string]bool   // versions qui démarrent
	fichiers   map[string][]byte // fichiers du conteneur
	appels     []string
	echecPull  bool
}

func (d *dockerSimule) Lancer(_ context.Context, args ...string) (string, error) {
	d.appels = append(d.appels, strings.Join(args, " "))
	switch {
	case args[0] == "pull":
		if d.echecPull {
			return "", errors.New("refusé")
		}
	case args[0] == "cp":
		src, dst := args[1], args[2]
		if c, ok := strings.CutPrefix(src, d.r.Conteneur+":"); ok { // conteneur → hôte
			b, ok := d.fichiers[c]
			if !ok {
				return "", errors.New("absent")
			}
			return "", os.WriteFile(dst, b, 0o600)
		}
		c, _ := strings.CutPrefix(dst, d.r.Conteneur+":")
		b, err := os.ReadFile(src)
		if err != nil {
			return "", err
		}
		d.fichiers[c] = b
	case len(args) >= 3 && args[0] == "compose" && args[1] == "up":
		env, _ := lireEnv(filepath.Join(d.r.DossierCompose, ".env"))
		v := valeurEnv(env, "MIRADOR_VERSION")
		if d.demarre[v] {
			if err := EcrireJSON(d.r.DossierDemande, NomPresence, Presence{Version: v, DemarreLe: d.maintenant().Unix()}, 0o644); err != nil {
				d.t.Fatal(err)
			}
			// La nouvelle version « migre » la base.
			d.fichiers[d.r.FichierBase] = append(d.fichiers[d.r.FichierBase], []byte("+"+v)...)
		}
	}
	return "", nil
}

type banc struct {
	h       *Hote
	d       *dockerSimule
	cle     ed25519.PrivateKey
	mfs     map[string][]byte // manifestes « publiés » (adresse → contenu)
	horloge time.Time
}

func nouveauBanc(t *testing.T) *banc {
	t.Helper()
	racine := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := ReglagesDefaut()
	r.DossierCompose = filepath.Join(racine, "compose")
	r.DossierDemande = filepath.Join(racine, "compose", "maj")
	r.DossierEtat = filepath.Join(racine, "compose", "maj-etat")
	r.DossierSauvegardes = filepath.Join(racine, "sauvegardes")
	r.ClesPubliques = []string{base64.StdEncoding.EncodeToString(pub)}
	r.Delai = 30 * time.Second
	for _, d := range []string{r.DossierDemande, r.DossierEtat, r.DossierSauvegardes} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.DossierCompose, ".env"), []byte("OLVID_ADMIN_CLIENT_KEY_CLI=secret\nMIRADOR_IMAGE=ghcr.io/prohand/mirador-server:0.5.0\nMIRADOR_VERSION=0.5.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &banc{cle: priv, mfs: map[string][]byte{}, horloge: time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC)}
	maintenant := func() time.Time { return b.horloge }
	b.d = &dockerSimule{t: t, r: r, maintenant: maintenant, demarre: map[string]bool{"0.5.0": true, "0.6.0": true},
		fichiers: map[string][]byte{r.FichierBase: []byte("base-0.5.0"), r.FichierBase + "-wal": []byte("wal")}}
	b.h = &Hote{R: r, Docker: b.d, Maintenant: maintenant, Dormir: func(d time.Duration) { b.horloge = b.horloge.Add(d) },
		Telecharger: func(_ context.Context, adresse string, _ int64) ([]byte, error) {
			if m, ok := b.mfs[adresse]; ok {
				return m, nil
			}
			return nil, errors.New("404")
		}}
	return b
}

const empreinteEssai = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func (b *banc) publier(t *testing.T, version string) {
	m, err := release.SignerImage(b.cle, empreinteEssai, version)
	if err != nil {
		t.Fatal(err)
	}
	brut, _ := json.Marshal(m)
	b.mfs["https://github.com/prohand/mirador-stack/releases/download/v"+version+"/mirador-server-image-"+version+".manifeste.json"] = brut
}

func (b *banc) demander(t *testing.T, version string) {
	if err := EcrireJSON(b.h.R.DossierDemande, NomDemande, Demande{Version: version, Le: 1, Par: "admin\n"}, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (b *banc) etat(t *testing.T) Etat {
	var e Etat
	if err := LireJSON(filepath.Join(b.h.R.DossierEtat, NomEtat), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAppliquerReussi(t *testing.T) {
	b := nouveauBanc(t)
	b.publier(t, "0.6.0")
	b.demander(t, "0.6.0")
	if err := b.h.Appliquer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := b.etat(t); e.Etat != EtatOK || e.Avant != "0.5.0" || e.Version != "0.6.0" {
		t.Fatalf("état : %+v", e)
	}
	env, _ := os.ReadFile(filepath.Join(b.h.R.DossierCompose, ".env"))
	if !strings.Contains(string(env), "MIRADOR_IMAGE=ghcr.io/prohand/mirador-server@"+empreinteEssai) ||
		!strings.Contains(string(env), "MIRADOR_VERSION=0.6.0") || !strings.Contains(string(env), "OLVID_ADMIN_CLIENT_KEY_CLI=secret") {
		t.Errorf(".env : %s", env)
	}
	if v, _ := b.h.VersionInstallee(); v != "0.6.0" {
		t.Errorf("version installée : %q", v)
	}
	if _, err := os.Stat(filepath.Join(b.h.R.DossierDemande, NomDemande)); !os.IsNotExist(err) {
		t.Error("demande pas effacée")
	}
	attendu := []string{"pull ghcr.io/prohand/mirador-server@" + empreinteEssai, "compose stop mirador-server", "cp mirador-server:/var/lib/mirador/mirador.db"}
	for i, a := range attendu {
		if i >= len(b.d.appels) || !strings.HasPrefix(b.d.appels[i], a) {
			t.Fatalf("appels : %q", b.d.appels)
		}
	}
	// Plus rien à faire : aucune commande.
	n := len(b.d.appels)
	if err := b.h.Appliquer(context.Background()); err != nil || len(b.d.appels) != n {
		t.Error("deuxième passage sans demande : des commandes lancées")
	}
}

func TestAppliquerRefus(t *testing.T) {
	for _, c := range []struct{ nom, version string }{{"plus ancienne", "0.4.9"}, {"même version", "0.5.0"}, {"format", "0.6.0-rc1"}, {"non signée", "0.7.0"}} {
		t.Run(c.nom, func(t *testing.T) {
			b := nouveauBanc(t)
			b.publier(t, "0.4.9")
			b.demander(t, c.version)
			if err := b.h.Appliquer(context.Background()); err == nil {
				t.Fatal("demande acceptée")
			}
			if e := b.etat(t); e.Etat != EtatRefuse {
				t.Errorf("état : %+v", e)
			}
			if len(b.d.appels) != 0 {
				t.Errorf("commandes lancées : %q", b.d.appels)
			}
		})
	}
	// Signée par une autre clé : refusée.
	b := nouveauBanc(t)
	_, autre, _ := ed25519.GenerateKey(rand.Reader)
	b.cle = autre
	b.publier(t, "0.6.0")
	b.demander(t, "0.6.0")
	if err := b.h.Appliquer(context.Background()); err == nil || len(b.d.appels) != 0 {
		t.Errorf("signature d'une autre clé acceptée (%v, %q)", err, b.d.appels)
	}
}

// La nouvelle version ne démarre pas : ancienne image, base d'avant (la
// « migration » est annulée), état échec.
func TestAppliquerRetourArriere(t *testing.T) {
	b := nouveauBanc(t)
	b.d.demarre["0.6.0"] = false
	b.d.fichiers[b.h.R.FichierBase] = []byte("base-0.5.0")
	b.publier(t, "0.6.0")
	b.demander(t, "0.6.0")
	// La « nouvelle version » abîme la base avant de planter.
	b.d.demarre["0.6.0"] = false
	err := b.h.Appliquer(context.Background())
	if err == nil {
		t.Fatal("échec non signalé")
	}
	e := b.etat(t)
	if e.Etat != EtatEchec || !strings.Contains(e.Message, "retour à 0.5.0 fait") {
		t.Fatalf("état : %+v", e)
	}
	env, _ := os.ReadFile(filepath.Join(b.h.R.DossierCompose, ".env"))
	if !strings.Contains(string(env), "MIRADOR_VERSION=0.5.0") || strings.Contains(string(env), "@sha256") {
		t.Errorf(".env pas remis : %s", env)
	}
	// Base : copie d'avant remise puis « migrée » par le redémarrage de 0.5.0 seulement.
	if got := string(b.d.fichiers[b.h.R.FichierBase]); got != "base-0.5.0+0.5.0" {
		t.Errorf("base : %q", got)
	}
	if string(b.d.fichiers[b.h.R.FichierBase+"-wal"]) != "wal" || len(b.d.fichiers[b.h.R.FichierBase+"-shm"]) != 0 {
		t.Error("journal WAL pas remis tel qu'il était")
	}
	if v, _ := b.h.VersionInstallee(); v != "0.5.0" {
		t.Errorf("version installée : %q", v)
	}
}

// Fichiers d'échange : un lien symbolique posé par le conteneur est refusé.
func TestDemandeLienRefusee(t *testing.T) {
	b := nouveauBanc(t)
	cible := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(cible, []byte(`{"version":"0.6.0"}`), 0o600)
	if err := os.Symlink(cible, filepath.Join(b.h.R.DossierDemande, NomDemande)); err != nil {
		t.Fatal(err)
	}
	if err := b.h.Appliquer(context.Background()); err == nil || len(b.d.appels) != 0 {
		t.Fatal("demande par lien symbolique acceptée")
	}
	if _, err := os.Lstat(filepath.Join(b.h.R.DossierDemande, NomDemande)); !os.IsNotExist(err) {
		t.Error("lien pas effacé")
	}
	if _, err := os.Stat(cible); err != nil {
		t.Error("cible du lien touchée")
	}
}

func TestReglages(t *testing.T) {
	r, err := LireReglages(strings.NewReader("# essai\nDOSSIER_COMPOSE=/srv/mirador\nCLES_PUBLIQUES=AAAA, BBBB\nDELAI=2m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.DossierDemande != "/srv/mirador/maj" || r.DossierEtat != "/srv/mirador/maj-etat" || len(r.ClesPubliques) != 2 || r.Delai != 2*time.Minute {
		t.Errorf("réglages : %+v", r)
	}
	for _, mauvais := range []string{"INCONNU=1\nCLES_PUBLIQUES=A", "DOSSIER_COMPOSE=relatif\nCLES_PUBLIQUES=A", "", "IMAGE=ghcr.io/x/y:latest\nCLES_PUBLIQUES=A", "DELAI=1s\nCLES_PUBLIQUES=A"} {
		if _, err := LireReglages(strings.NewReader(mauvais)); err == nil {
			t.Errorf("accepté : %q", mauvais)
		}
	}
}

func TestComparer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		r    int
	}{{"0.5.0", "0.4.10", 1}, {"0.10.0", "0.9.9", 1}, {"1.0.0", "1.0.0", 0}, {"0.5.1", "0.6.0", -1}} {
		if got := Comparer(c.a, c.b); got != c.r {
			t.Errorf("Comparer(%s, %s) = %d", c.a, c.b, got)
		}
	}
}
