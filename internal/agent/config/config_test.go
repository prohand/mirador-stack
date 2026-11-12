// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fichier minimal valide, réutilisé par plusieurs tests.
const fichierValide = `
# Configuration de test
MIRADOR_NOM=srv-test-mirador
MIRADOR_SITE=bureau
MIRADOR_ENV="vm"
MIRADOR_URL_METRIQUES=https://ingest.example.org/api/v1/write
`

func TestLireValide(t *testing.T) {
	a, err := Lire(strings.NewReader(fichierValide))
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if a.Nom != "srv-test-mirador" || a.Env != "vm" {
		t.Errorf("valeurs mal lues : %+v", a)
	}
	// Les clés absentes gardent leur valeur par défaut.
	if a.Intervalle != 30*time.Second || a.TamponMetriques != 8<<20 {
		t.Errorf("défauts non appliqués : %+v", a)
	}
}

func TestLireRefus(t *testing.T) {
	cas := map[string]string{
		"clé inconnue":      fichierValide + "MIRADOR_COMMANDE=rm -rf /\n",
		"clé en double":     fichierValide + "MIRADOR_SITE=autre\n",
		"nom générique":     strings.Replace(fichierValide, "srv-test-mirador", "ubuntu", 1),
		"nom invalide":      strings.Replace(fichierValide, "srv-test-mirador", "a b", 1),
		"http distant":      strings.Replace(fichierValide, "https://ingest", "http://ingest", 1),
		"identifiant URL":   strings.Replace(fichierValide, "https://ingest", "https://moi:secret@ingest", 1),
		"intervalle court":  fichierValide + "MIRADOR_INTERVALLE=1s\n",
		"tampon trop grand": fichierValide + "MIRADOR_TAMPON_METRIQUES=999999999\n",
		"regex invalide":    fichierValide + "MIRADOR_EXCLURE_DISQUES=([\n",
		"ligne sans égal":   fichierValide + "n'importe quoi\n",
		"niveau inconnu":    fichierValide + "MIRADOR_NIVEAU_LOG=bavard\n",
		"token relatif":     fichierValide + "MIRADOR_FICHIER_TOKEN=token\n",
		"lent < intervalle": fichierValide + "MIRADOR_INTERVALLE_LENT=15s\n",
		"durée mal formée":  fichierValide + "MIRADOR_INTERVALLE=trente\n",
		"site absent":       strings.Replace(fichierValide, "MIRADOR_SITE=bureau\n", "", 1),
		"schéma inattendu":  strings.Replace(fichierValide, "https://ingest", "file://ingest", 1),
	}
	for nom, contenu := range cas {
		t.Run(nom, func(t *testing.T) {
			if _, err := Lire(strings.NewReader(contenu)); err == nil {
				t.Errorf("le fichier aurait dû être refusé")
			}
		})
	}
}

func TestHTTPLocalAccepte(t *testing.T) {
	contenu := strings.Replace(fichierValide, "https://ingest.example.org", "http://127.0.0.1:8428", 1)
	if _, err := Lire(strings.NewReader(contenu)); err != nil {
		t.Fatalf("http vers 127.0.0.1 devrait être accepté : %v", err)
	}
}

func TestLireToken(t *testing.T) {
	dir := t.TempDir()
	chemin := filepath.Join(dir, "token")

	if err := os.WriteFile(chemin, []byte("0123456789abcdef0123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := LireToken(chemin)
	if err != nil || tok != "0123456789abcdef0123" {
		t.Fatalf("token mal lu : %q, %v", tok, err)
	}

	// Lisible par tous : refusé.
	if err := os.Chmod(chemin, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LireToken(chemin); err == nil {
		t.Error("un token lisible par tous aurait dû être refusé")
	}

	// Trop court : refusé.
	if err := os.WriteFile(chemin, []byte("court"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(chemin, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LireToken(chemin); err == nil {
		t.Error("un token trop court aurait dû être refusé")
	}
}

// FuzzLire : le lecteur ne doit jamais paniquer, quel que soit le contenu.
func FuzzLire(f *testing.F) {
	f.Add(fichierValide)
	f.Add("MIRADOR_INTERVALLE=\nMIRADOR_TAMPON_METRIQUES=-1")
	f.Fuzz(func(t *testing.T, contenu string) {
		a, err := Lire(strings.NewReader(contenu))
		if err == nil {
			// Toute configuration acceptée doit être valide.
			if err := a.Valider(); err != nil {
				t.Fatalf("configuration acceptée mais invalide : %v", err)
			}
		}
	})
}

func TestReglagesPartie3(t *testing.T) {
	a, err := Lire(strings.NewReader(fichierValide + `MIRADOR_SYSTEMD=non
MIRADOR_SYSTEMD_IGNORER=fwupd-refresh.service, mon-appli.service
MIRADOR_MISES_A_JOUR=oui
MIRADOR_DOCKER=http://127.0.0.1:2375
`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Systemd || !a.MisesAJour || a.Docker != "http://127.0.0.1:2375" || len(a.SystemdIgnorer) != 2 {
		t.Errorf("réglages mal lus : %+v", a)
	}
	for _, mauvais := range []string{
		"MIRADOR_DOCKER=http://10.0.0.5:2375",       // proxy distant refusé
		"MIRADOR_DOCKER=http://127.0.0.1",           // port obligatoire
		"MIRADOR_DOCKER=tcp://127.0.0.1:2375",       // schéma inconnu
		"MIRADOR_DOCKER=unix://relatif/docker.sock", // chemin relatif
		"MIRADOR_SYSTEMD=peut-être",
		"MIRADOR_SYSTEMD_IGNORER=pas un service",
	} {
		if _, err := Lire(strings.NewReader(fichierValide + mauvais + "\n")); err == nil {
			t.Errorf("aurait dû être refusé : %s", mauvais)
		}
	}
	if _, err := Lire(strings.NewReader(fichierValide + "MIRADOR_DOCKER=unix:///run/docker-proxy.sock\n")); err != nil {
		t.Errorf("socket Unix absolue refusée : %v", err)
	}
}

// Site et env : facultatifs avec mirador-server (choisis dans l'interface),
// obligatoires sinon.
func TestSiteEnvFacultatifsAvecMirador(t *testing.T) {
	avec := "MIRADOR_NOM=srv-test\nMIRADOR_SERVEUR=https://ingest.example.org\nMIRADOR_FICHIER_TOKEN=/var/lib/mirador-agent/token\n"
	a, err := Lire(strings.NewReader(avec))
	if err != nil || a.Site != "" || a.Env != "" {
		t.Fatalf("mode mirador sans site ni env : %v", err)
	}
	if _, err := Lire(strings.NewReader("MIRADOR_NOM=srv-test\nMIRADOR_URL_METRIQUES=https://ingest.example.org/api/v1/write\n")); err == nil {
		t.Error("site et env vides acceptés sans mirador-server")
	}
	if _, err := Lire(strings.NewReader(avec + "MIRADOR_SITE=pas bon\n")); err == nil {
		t.Error("site invalide accepté")
	}
}

func TestMetriquesReglables(t *testing.T) {
	a, err := Lire(strings.NewReader(fichierValide + "MIRADOR_METRIQUES=non\n"))
	if err != nil || a.Metriques {
		t.Fatalf("MIRADOR_METRIQUES=non : %v %v", a.Metriques, err)
	}
	if d := Defaut(); !d.Metriques {
		t.Error("métriques désactivées par défaut")
	}
}

// L'empreinte de la machine : stable, propre à la machine, et jamais
// l'identifiant lui-même.
func TestEmpreinteMachine(t *testing.T) {
	dir := t.TempDir()
	ecrire := func(nom, contenu string) string {
		p := filepath.Join(dir, nom)
		if err := os.WriteFile(p, []byte(contenu), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := EmpreinteMachine(ecrire("a", "0123456789abcdef0123456789abcdef\n"))
	b := EmpreinteMachine(ecrire("b", "fedcba9876543210fedcba9876543210\n"))
	if len(a) != 32 || a == b || a != EmpreinteMachine(filepath.Join(dir, "a")) {
		t.Errorf("empreintes : %q %q", a, b)
	}
	if strings.Contains(a, "0123456789abcdef") {
		t.Error("l'identifiant de la machine ne doit pas sortir tel quel")
	}
	for _, mauvais := range []string{ecrire("vide", ""), ecrire("court", "abc\n"), filepath.Join(dir, "absent")} {
		if e := EmpreinteMachine(mauvais); e != "" {
			t.Errorf("%s : empreinte %q attendue vide", mauvais, e)
		}
	}
	// Jamais envoyée ailleurs que vers mirador-server.
	sans := Agent{}
	if h := sans.EntetesMachine(map[string]string{}); len(h) != 0 {
		t.Errorf("en-tête envoyé sans mirador-server : %v", h)
	}
}

func TestMotifDistant(t *testing.T) {
	for _, m := range []string{"/etc/nginx/ssl/*.crt", "/etc/letsencrypt/live/*/cert.pem", "/opt/appli/tls/serveur.pem",
		"/srv/certs/*.cer", "/var/lib/traefik/certs/*.cert", "/usr/local/etc/x.pem"} {
		if err := ValiderMotifDistant(m); err != nil {
			t.Errorf("%s refusé : %v", m, err)
		}
	}
	for _, m := range []string{"/etc/shadow", "/etc/ssl/*", "/proc/self/x.pem", "/home/a/x.pem", "/root/.acme.sh/x.cer",
		"/tmp/x.pem", "/etc/../x.pem", "/etc//x.pem", "/etc/ssl/private/privkey.pem", "/etc/ssl/serveur.key.pem",
		"/etc/*/*/*/*.pem", "etc/x.pem", "/etc/x.pem\x00"} {
		if err := ValiderMotifDistant(m); err == nil {
			t.Errorf("%q accepté à tort", m)
		}
	}
	if !FichierAccepteDistant("/etc/ssl/serveur.pem") || FichierAccepteDistant("/etc/ssl/privkey.pem") {
		t.Error("FichierAccepteDistant")
	}
}
