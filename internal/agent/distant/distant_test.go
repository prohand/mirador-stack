// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/protocole"
)

func base() config.Agent {
	c := config.Defaut()
	c.Nom, c.Site, c.Env = "srv-test-mirador", "bureau", "vm"
	c.URLMetriques = "https://ingest.example.org/api/v1/write"
	return c
}

func TestAppliquer(t *testing.T) {
	d, err := Decoder(strings.NewReader(`{"intervalle":"60s","intervalle_lent":"120s","systemd":false,"logs_debit":50,"logs_rafale":100,"masquage":["mots_de_passe"]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Appliquer(base(), d)
	if err != nil {
		t.Fatal(err)
	}
	if a.Intervalle != time.Minute || a.Systemd || a.LogsDebit != 50 || len(a.Masquage) != 1 {
		t.Errorf("valeurs non appliquées : %+v", a)
	}
	if a.URLMetriques != base().URLMetriques || a.Nom != base().Nom {
		t.Error("les valeurs absentes doivent rester inchangées")
	}
}

// Réglages de surveillance choisis dans l'interface (SPEC § 19).
func TestAppliquerSurveillance(t *testing.T) {
	d, err := Decoder(strings.NewReader(`{"temperatures":false,"ventilateurs":true,"zfs":true,"mdraid":true,
		"docker_metriques":false,"envoyer_smart":false,"systemd_validation":true,"systemd_surveiller":["ssh.service","nginx.service"]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Appliquer(base(), d)
	if err != nil {
		t.Fatal(err)
	}
	if a.Temperatures || !a.Ventilateurs || !a.ZFS || !a.Mdraid || a.DockerMetriques || a.EnvoyerSmart ||
		!a.SystemdValidation || len(a.SystemdSurveiller) != 2 {
		t.Errorf("valeurs non appliquées : %+v", a)
	}
	if !a.EnvoyerPareFeu || !a.EnvoyerCertificats {
		t.Error("les valeurs absentes gardent leur défaut (envoyé)")
	}
	// Défauts : ce qui était collecté le reste, les nouveautés sont éteintes.
	b := base()
	if !b.Temperatures || !b.DockerMetriques || !b.EnvoyerSmart || b.Ventilateurs || b.ZFS || b.Mdraid || b.SystemdValidation {
		t.Errorf("défauts inattendus : %+v", b)
	}
	// Un nom de service qui n'en est pas un est refusé.
	d, _ = Decoder(strings.NewReader(`{"systemd_surveiller":["rm -rf /"]}`))
	if _, err := Appliquer(base(), d); err == nil {
		t.Error("nom de service invalide accepté")
	}
}

func TestRefus(t *testing.T) {
	cas := map[string]string{
		// Champs qui n'existent pas dans le schéma : refusés en bloc.
		"commande":        `{"commande":"rm -rf /"}`,
		"script":          `{"intervalle":"30s","script":"curl x | sh"}`,
		"chemin":          `{"fichier_token":"/tmp/x"}`,
		"adresse serveur": `{"url_metriques":"https://pirate.example"}`,
		// Champs connus mais valeurs hors bornes : refusés par la validation.
		"intervalle":       `{"intervalle":"1s"}`,
		"durée sans unité": `{"intervalle":30}`,
		"regex invalide":   `{"exclure_disques":"(["}`,
		"masquage inconnu": `{"masquage":["tout"]}`,
		"récupération":     `{"recuperation":"10s"}`,
		"deux documents":   `{} {}`,
	}
	for nom, doc := range cas {
		t.Run(nom, func(t *testing.T) {
			d, err := Decoder(strings.NewReader(doc))
			if err == nil {
				_, err = Appliquer(base(), d)
			}
			if err == nil {
				t.Error("document accepté alors qu'il aurait dû être refusé")
			}
		})
	}
}

func TestRecuperateurETag(t *testing.T) {
	var appels atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appels.Add(1)
		if r.Header.Get("Authorization") != "Bearer jeton" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(`{"intervalle":"45s"}`))
	}))
	defer srv.Close()
	r := &Recuperateur{URL: srv.URL, Token: "jeton", Client: srv.Client()}
	c, err := r.Recuperer(context.Background())
	if err != nil || c.Intervalle == nil || time.Duration(*c.Intervalle) != 45*time.Second {
		t.Fatalf("1re récupération : %+v %v", c, err)
	}
	if _, err := r.Recuperer(context.Background()); !errors.Is(err, ErrInchange) {
		t.Errorf("2e récupération : ETag non utilisé (%v)", err)
	}
}

func TestEnrolement(t *testing.T) {
	var appels atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var d protocole.DemandeEnrolement
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Jeton != "jeton-usage-unique" || len(d.Identifiant) != 32 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rep := protocole.ReponseEnrolement{Statut: protocole.EnAttente}
		if appels.Add(1) >= 3 { // approuvée au 3e passage
			rep = protocole.ReponseEnrolement{Statut: protocole.Approuve, Token: strings.Repeat("t", 48)}
		}
		_ = json.NewEncoder(w).Encode(rep)
	}))
	defer srv.Close()

	fichier := filepath.Join(t.TempDir(), "token")
	e := &Enrolement{URL: srv.URL, Jeton: "jeton-usage-unique", NomPropose: "srv-test",
		FichierToken: fichier, Client: srv.Client(), AttenteMin: 10 * time.Millisecond}
	ctx, annuler := context.WithTimeout(context.Background(), 5*time.Second)
	defer annuler()
	if err := e.Executer(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fichier)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token mal enregistré : %v %v", info, err)
	}
	if b, _ := os.ReadFile(fichier); strings.TrimSpace(string(b)) != strings.Repeat("t", 48) {
		t.Error("contenu du token incorrect")
	}
}

func TestEnrolementRefuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(protocole.ReponseEnrolement{Statut: protocole.Refuse, Message: "jeton expiré"})
	}))
	defer srv.Close()
	e := &Enrolement{URL: srv.URL, Jeton: "x", FichierToken: filepath.Join(t.TempDir(), "token"), Client: srv.Client()}
	if err := e.Executer(context.Background()); !errors.Is(err, ErrRefuse) {
		t.Errorf("refus attendu : %v", err)
	}
}

func FuzzDecoder(f *testing.F) {
	f.Add(`{"intervalle":"30s","masquage":["cartes"]}`)
	f.Fuzz(func(t *testing.T, doc string) {
		d, err := Decoder(strings.NewReader(doc))
		if err != nil {
			return
		}
		if a, err := Appliquer(base(), d); err == nil {
			if err := a.Valider(); err != nil {
				t.Fatalf("configuration appliquée mais invalide : %v", err)
			}
		}
	})
}

func TestDistantMetriques(t *testing.T) {
	base := config.Defaut()
	base.Nom, base.Site, base.Env = "pc", "s", "e"
	base.URLMetriques = "https://exemple.invalid/api/agent/v1/metrics"
	non := false
	a, err := Appliquer(base, &ConfigDistante{Metriques: &non})
	if err != nil || a.Metriques {
		t.Fatalf("metriques distantes : %v %v", a.Metriques, err)
	}
}
