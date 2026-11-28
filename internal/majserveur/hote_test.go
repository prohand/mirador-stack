// SPDX-License-Identifier: GPL-3.0-or-later

package majserveur

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	liens      map[string]string // liens symboliques du conteneur (chemin → cible)
	appels     []string
	echecPull  bool
	images     []*imageSimulee // nil : commandes « image » en échec
	utilisees  map[string]bool // images utilisées par un conteneur (refus de suppression)
}

type imageSimulee struct {
	id, digest string
	tags       []string
}

// trouver : image désignée par « IMAGE:tag » ou « IMAGE@digest ».
func (d *dockerSimule) trouver(ref string) *imageSimulee {
	for _, im := range d.images {
		if _, dg, ok := strings.Cut(ref, "@"); ok && im.digest == dg {
			return im
		}
		for _, t := range im.tags {
			if ref == d.r.Image+":"+t {
				return im
			}
		}
	}
	return nil
}

func (d *dockerSimule) lancerImage(args []string) (string, error) {
	if d.images == nil {
		return "", errors.New("pas d'image simulée")
	}
	switch {
	case args[0] == "tag":
		im := d.trouver(args[1])
		if im == nil {
			return "", errors.New("absente")
		}
		tag := strings.TrimPrefix(args[2], d.r.Image+":")
		for _, x := range d.images {
			x.tags = slices.DeleteFunc(x.tags, func(t string) bool { return t == tag })
		}
		im.tags = append(im.tags, tag)
	case args[1] == "inspect":
		if im := d.trouver(args[len(args)-1]); im != nil {
			return im.id + "\n", nil
		}
		return "", errors.New("absente")
	case args[1] == "ls":
		var b strings.Builder
		b.WriteString("WARNING: ligne parasite\n")
		for _, im := range d.images {
			if len(im.tags) == 0 {
				fmt.Fprintf(&b, "%s|<none>|%s\n", im.id, im.digest)
			}
			for _, t := range im.tags {
				fmt.Fprintf(&b, "%s|%s|%s\n", im.id, t, im.digest)
			}
		}
		return b.String(), nil
	case args[1] == "rm":
		ref := args[2]
		im := d.trouver(ref)
		if im == nil {
			return "", errors.New("absente")
		}
		if tag, ok := strings.CutPrefix(ref, d.r.Image+":"); ok && len(im.tags) > 1 {
			im.tags = slices.DeleteFunc(im.tags, func(t string) bool { return t == tag })
			return "", nil
		}
		if d.utilisees[im.id] {
			return "", errors.New("conflict: image is being used by running container")
		}
		d.images = slices.DeleteFunc(d.images, func(x *imageSimulee) bool { return x == im })
	}
	return "", nil
}

func (d *dockerSimule) Lancer(_ context.Context, args ...string) (string, error) {
	d.appels = append(d.appels, strings.Join(args, " "))
	switch {
	case args[0] == "tag" || args[0] == "image":
		return d.lancerImage(args)
	case args[0] == "pull":
		if d.echecPull {
			return "", errors.New("refusé")
		}
		if _, dg, ok := strings.Cut(args[1], "@"); ok && d.images != nil && d.trouver(args[1]) == nil {
			d.images = append(d.images, &imageSimulee{id: "sha256:" + strings.Repeat("c", 64), digest: dg})
			d.utilisees["sha256:"+strings.Repeat("c", 64)] = true
		}
	case args[0] == "cp":
		src, dst := args[1], args[2]
		if c, ok := strings.CutPrefix(src, d.r.Conteneur+":"); ok { // conteneur → hôte
			if cible, ok := d.liens[c]; ok { // docker cp recopie le lien tel quel
				return "", os.Symlink(cible, dst)
			}
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
		fichiers: map[string][]byte{r.FichierBase: []byte("base-0.5.0"), r.FichierBase + "-wal": []byte("wal")}, utilisees: map[string]bool{}}
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

// Réglages publiés pour l'interface : empreintes des clés (pas les clés),
// version notée, problèmes.
func TestEcrireInfo(t *testing.T) {
	b := nouveauBanc(t)
	if err := b.h.EcrireInfo("0.6.0", []string{"volume absent"}); err != nil {
		t.Fatal(err)
	}
	var i InfoService
	if err := LireJSON(filepath.Join(b.h.R.DossierEtat, NomService), &i); err != nil {
		t.Fatal(err)
	}
	if i.DossierCompose != b.h.R.DossierCompose || i.Installee != "0.5.0" || len(i.Cles) != 1 || len(i.Cles[0]) != 16 || len(i.Problemes) != 1 {
		t.Errorf("info : %+v", i)
	}
}

// Retour d'Arcane (21/11) : après la mise à jour, l'image porte sa version,
// la précédente aussi (retour possible) ; « latest » et les anciennes
// images sont retirées ; une image utilisée n'est jamais forcée.
func TestAppliquerRangeImages(t *testing.T) {
	b := nouveauBanc(t)
	id := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	b.d.images = []*imageSimulee{
		{id: id("1"), digest: "sha256:" + strings.Repeat("1", 64), tags: []string{"latest"}},         // première installation (0.4.0)
		{id: id("2"), digest: "sha256:" + strings.Repeat("2", 64), tags: []string{"0.5.0"}},          // version en service avant
		{id: id("3"), digest: "sha256:" + strings.Repeat("3", 64)},                                   // ancienne, sans étiquette
		{id: id("4"), digest: "sha256:" + strings.Repeat("4", 64), tags: []string{"0.3.0", "vieux"}}, // deux étiquettes
	}
	b.publier(t, "0.6.0")
	b.demander(t, "0.6.0")
	if err := b.h.Appliquer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := b.etat(t); e.Etat != EtatOK {
		t.Fatalf("état : %+v", e)
	}
	var restent []string
	for _, im := range b.d.images {
		restent = append(restent, im.id[7:8]+":"+strings.Join(im.tags, ","))
	}
	if strings.Join(restent, " ") != "2:0.5.0 c:0.6.0" {
		t.Errorf("images restantes : %v (appels : %q)", restent, b.d.appels)
	}
	// Une image utilisée par un autre conteneur est gardée (jamais forcée).
	b2 := nouveauBanc(t)
	b2.d.images = []*imageSimulee{{id: id("9"), digest: "sha256:" + strings.Repeat("9", 64), tags: []string{"latest"}}}
	b2.d.utilisees[id("9")] = true
	b2.publier(t, "0.6.0")
	b2.demander(t, "0.6.0")
	if err := b2.h.Appliquer(context.Background()); err != nil || b2.etat(t).Etat != EtatOK {
		t.Fatalf("mise à jour : %v %+v", err, b2.etat(t))
	}
	if len(b2.d.images) != 2 {
		t.Errorf("image utilisée supprimée : %d image(s)", len(b2.d.images))
	}
}
