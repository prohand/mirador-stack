// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Canal webhook (SPEC § 81) : formats, en-tête secret, adresse chiffrée,
// secret à ressaisir si l'adresse change, réseau local.
func TestWebhook(t *testing.T) {
	var mu sync.Mutex
	var recus []*http.Request
	var corps []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		recus, corps = append(recus, r), append(corps, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	m := &Message{Le: t0, Type: TypeAlerte, Gravite: "critical", Nouvelles: 1,
		Actives: []alertes.Alerte{alerte(7, "DisquePresquePlein", "critical", "nas", t0.Add(-time.Hour), "mountpoint", "/")}}

	// Sans « réseau local permis » : http:// refusé à la validation.
	w := Webhook{Actif: true, URL: srv.URL + "/api/webhook/x", Format: FormatJSON}
	if err := w.valider(); err == nil {
		t.Fatal("http:// accepté sans réseau local")
	}
	w.ReseauPrive = true
	w.Secret = "s3cret"
	cn := CanauxDefaut()
	cn.Principal, cn.Webhook = NomWebhook, &w
	prep, err := PreparerSecrets(cn, Canaux{}, cleTest)
	if err != nil || strings.Contains(prep.Webhook.URL, "/api/webhook") || strings.Contains(prep.Webhook.Secret, "s3cret") {
		t.Fatalf("secrets : %v %+v", err, prep.Webhook)
	}
	if err := prep.Valider(true); err != nil {
		t.Fatal(err)
	}
	if mq := prep.Masques(); mq.Webhook.URL != "***" || mq.Webhook.Secret != "***" {
		t.Errorf("masques : %+v", mq.Webhook)
	}
	canaux, err := fabriquer(&prep, cleTest)
	if err != nil {
		t.Fatal(err)
	}
	c := canaux[NomWebhook]
	if err := c.Verifier(context.Background()); err != nil {
		t.Errorf("vérifier : %v", err)
	}
	if err := c.Envoyer(context.Background(), m, Mise{Lien: "https://mirador.exemple.fr"}); err != nil {
		t.Fatal(err)
	}
	var j struct {
		Type, Gravite, Titre string
		Nb                   int
		Alertes              []struct {
			Regle, Machine, Etat, Lien string
			Etiquettes                 map[string]string
		}
	}
	if err := json.Unmarshal([]byte(corps[0]), &j); err != nil {
		t.Fatal(err)
	}
	if j.Type != "alerte" || j.Nb != 1 || j.Titre != "mirador : DisquePresquePlein nas" || len(j.Alertes) != 1 ||
		j.Alertes[0].Lien != "https://mirador.exemple.fr/alertes/7" || j.Alertes[0].Etiquettes["mountpoint"] != "/" || j.Alertes[0].Etat != "en_cours" {
		t.Errorf("JSON : %s", corps[0])
	}
	if recus[0].Header.Get("Authorization") != "Bearer s3cret" || recus[0].Method != http.MethodPost {
		t.Errorf("en-tête : %v", recus[0].Header)
	}

	// Nouvelle adresse sans ressaisir l'en-tête secret : refusé.
	nv := prep
	w2 := *prep.Webhook
	w2.URL, w2.Secret = srv.URL+"/autre", "***"
	nv.Webhook = &w2
	if _, err := PreparerSecrets(nv, prep, cleTest); err == nil || !strings.Contains(err.Error(), "en-tête secret") {
		t.Errorf("secret suivi vers une autre adresse : %v", err)
	}

	// Autres formats : forme du corps, méthode, chemin.
	for _, c := range []struct{ format, chemin, attendu, methode string }{
		{FormatDiscord, "/hook", `"content":"🔴 DisquePresquePlein`, http.MethodPost},
		{FormatSlack, "/hook", `"text":"🔴 DisquePresquePlein`, http.MethodPost},
		{FormatGotify, "/", `"priority":8`, http.MethodPost},
		{FormatMatrix, "/_matrix/client/v3/rooms/!a:b/send/m.room.message", `"msgtype":"m.text"`, http.MethodPut},
	} {
		wh := &webhook{url: srv.URL + c.chemin, format: c.format, secret: "k", client: clientHTTP(true), prive: true}
		if err := wh.Envoyer(context.Background(), m, Mise{}); err != nil {
			t.Fatalf("%s : %v", c.format, err)
		}
		mu.Lock()
		r, b := recus[len(recus)-1], corps[len(corps)-1]
		mu.Unlock()
		if !strings.Contains(b, c.attendu) || r.Method != c.methode {
			t.Errorf("%s : %s %s", c.format, r.Method, b)
		}
		switch c.format {
		case FormatGotify:
			if r.URL.Path != "/message" || r.Header.Get("X-Gotify-Key") != "k" {
				t.Errorf("gotify : %s %v", r.URL.Path, r.Header)
			}
		case FormatMatrix:
			if !strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!a:b/send/m.room.message/mirador") {
				t.Errorf("matrix : %s", r.URL.Path)
			}
		}
	}
	// Matrix : adresse sans /send/m.room.message refusée.
	if err := (&Webhook{Format: FormatMatrix, URL: "https://matrix.org/_matrix/client/v3/rooms/x"}).valider(); err == nil {
		t.Error("adresse Matrix incomplète acceptée")
	}
	// Adresse locale refusée sans réseau local (SSRF).
	wh := &webhook{url: srv.URL, format: FormatJSON, client: clientHTTP(false)}
	if err := wh.Envoyer(context.Background(), m, Mise{}); err == nil || !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("adresse locale : %v", err)
	}
}
