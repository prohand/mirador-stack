// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/secrets"
)

// canalSimule : canal dont on choisit la panne.
type canalSimule struct {
	nom   string
	panne atomic.Bool
	mu    sync.Mutex
	recus []string
}

func (c *canalSimule) Nom() string { return c.nom }

func (c *canalSimule) Envoyer(_ context.Context, m *Message, o Mise) error {
	if c.panne.Load() {
		return errors.New(c.nom + " bloqué (pare-feu simulé)")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recus = append(c.recus, Texte(m, o))
	return nil
}

func (c *canalSimule) Verifier(context.Context) error {
	if c.panne.Load() {
		return errors.New(c.nom + " injoignable")
	}
	return nil
}

func (c *canalSimule) vider() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.recus
	c.recus = nil
	return r
}

func contient(msgs []string, morceau string) bool {
	for _, m := range msgs {
		if strings.Contains(m, morceau) {
			return true
		}
	}
	return false
}

// Bout en bout de la bascule (brief : « bascule automatique testée,
// simulation de panne »).
func TestBasculeDeBoutEnBout(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	horloge := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r := ReglagesDefaut()
	r.Mode = Actif
	r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234"}
	r.Canaux.Ntfy = &Ntfy{Actif: true, URL: "https://ntfy.example.org/mirador"}
	r.Canaux.Secours = NomNtfy
	r.Canaux.Echecs, r.Canaux.Retours = 2, 2
	r.Canaux.VerifTelegram = alertes.Duree(time.Minute)
	if err := EcrireReglages(ctx, db, r, cleTest, "test", horloge); err != nil {
		t.Fatal(err)
	}
	tg, nt := &canalSimule{nom: NomTelegram}, &canalSimule{nom: NomNtfy}
	p, err := NouveauPostier(ctx, OptionsPostier{DB: db, Cle: cleTest, Maintenant: func() time.Time { return horloge },
		DerniereEvaluationOK: func() time.Time { return horloge },
		Fabrique: func(c *Canaux, _ []byte) (map[string]Canal, error) {
			return map[string]Canal{NomTelegram: tg, NomNtfy: nt}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	tour := func() {
		horloge = horloge.Add(time.Minute)
		p.Tour(ctx)
	}
	alerte := func(n string) *Message {
		a := alertes.Alerte{ID: 1, Regle: n, Gravite: alertes.Critique, Etat: alertes.Active, ActiveLe: horloge}
		return &Message{Le: horloge, Groupe: n, Gravite: alertes.Critique, Type: TypeAlerte, Actives: []alertes.Alerte{a}, Nouvelles: 1}
	}

	// 1. Tout va bien : Telegram.
	p.Envoyer(ctx, alerte("A1"), nil)
	if m := tg.vider(); !contient(m, "A1") || len(nt.vider()) != 0 {
		t.Fatalf("normal : %v", m)
	}

	// 2. Telegram bloqué : 2 vérifications en échec → panne, prévenu par ntfy
	// (une seule fois, même après d'autres échecs).
	tg.panne.Store(true)
	tour()
	if len(nt.vider()) != 0 {
		t.Fatal("panne annoncée après un seul échec (anti-rebond)")
	}
	tour()
	tour()
	m := nt.vider()
	if len(m) != 1 || !strings.Contains(m[0], "Canal telegram (principal) en panne") {
		t.Fatalf("message de panne : %v", m)
	}
	// Les alertes partent par ntfy, avec la mention du canal défaillant.
	p.Envoyer(ctx, alerte("A2"), nil)
	if m := nt.vider(); !contient(m, "A2") || !contient(m, "canal telegram en panne") {
		t.Fatalf("secours : %v", m)
	}

	// 3. ntfy tombe aussi : le message de panne essaie Telegram (en panne) →
	// gardé en file ; l'alerte suivante aussi.
	nt.panne.Store(true)
	tour()
	tour()
	p.Envoyer(ctx, alerte("A3"), nil)
	if n := p.TailleFile(ctx); n != 2 {
		t.Fatalf("file d'attente : %d message(s), 2 attendus", n)
	}

	// 4. Telegram revient : 2 vérifications réussies → retour annoncé, puis
	// la file est vidée dans l'ordre, avec l'heure d'origine.
	tg.panne.Store(false)
	tour()
	tour()
	tour()
	m = tg.vider()
	if !contient(m, "Canal telegram (principal) revenu") || !contient(m, "A3") || !contient(m, "En attente depuis le") ||
		!contient(m, "Canal ntfy (secours) en panne") {
		t.Fatalf("retour : %v", m)
	}
	if n := p.TailleFile(ctx); n != 0 {
		t.Fatalf("file non vidée : %d", n)
	}

	// 5. Historique : chaque essai est noté (ok, echec, en_file, secours).
	es, err := ListerEnvois(ctx, db, 100)
	if err != nil {
		t.Fatal(err)
	}
	vus := map[string]bool{}
	for _, e := range es {
		vus[e.Statut] = true
		if e.Secours {
			vus["secours"] = true
		}
	}
	for _, s := range []string{"ok", "echec", "en_file", "secours"} {
		if !vus[s] {
			t.Errorf("historique : aucun envoi %q", s)
		}
	}

	// 6. Inverser principal et secours : un réglage, sans toucher au code.
	r2, _ := LireReglages(ctx, db)
	r2.Canaux.Principal, r2.Canaux.Secours = NomNtfy, NomTelegram
	r2.Canaux.Telegram.Jeton = "***" // garder le secret
	if err := EcrireReglages(ctx, db, r2, cleTest, "test", horloge); err != nil {
		t.Fatal(err)
	}
	nt.panne.Store(false)
	tour()
	tour()
	tour()
	nt.vider()
	tg.vider()
	p.Envoyer(ctx, alerte("A4"), nil)
	if m := nt.vider(); !contient(m, "A4") || len(tg.vider()) != 0 {
		t.Fatalf("après inversion : ntfy %v", m)
	}

	// 7. Test d'envoi demandé par la commande.
	id, err := DemanderTest(ctx, db, NomTelegram, horloge)
	if err != nil {
		t.Fatal(err)
	}
	tour()
	if st, e, err := ResultatTest(ctx, db, id); err != nil || st != "ok" || !contient(tg.vider(), "Message de test") {
		t.Fatalf("test : %s %s %v", st, e, err)
	}
}

func TestCritiqueSurTousLesCanaux(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	r := ReglagesDefaut()
	r.Mode = Actif
	r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234"}
	r.Canaux.Ntfy = &Ntfy{Actif: true, URL: "https://ntfy.example.org/mirador"}
	r.Canaux.Tous = []string{alertes.Critique}
	if err := EcrireReglages(ctx, db, r, cleTest, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	tg, nt := &canalSimule{nom: NomTelegram}, &canalSimule{nom: NomNtfy}
	p, err := NouveauPostier(ctx, OptionsPostier{DB: db, Cle: cleTest, DerniereEvaluationOK: time.Now,
		Fabrique: func(*Canaux, []byte) (map[string]Canal, error) {
			return map[string]Canal{NomTelegram: tg, NomNtfy: nt}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	a := alertes.Alerte{Regle: "X", Gravite: alertes.Critique}
	p.Envoyer(ctx, &Message{Le: time.Now(), Gravite: alertes.Critique, Actives: []alertes.Alerte{a}}, nil)
	p.Envoyer(ctx, &Message{Le: time.Now(), Gravite: alertes.Info, Actives: []alertes.Alerte{{Regle: "Y", Gravite: alertes.Info}}}, nil)
	if a, b := tg.vider(), nt.vider(); len(a) != 2 || len(b) != 1 || !contient(b, "X") {
		t.Fatalf("critique partout, info sur le principal : telegram %d, ntfy %d", len(a), len(b))
	}
}

func TestTelegramEtNtfyReels(t *testing.T) {
	ctx := context.Background()
	var recu map[string]any
	var chemin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chemin = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &recu)
		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	tg := &telegram{api: srv.URL, jeton: jetonTest, chat: "-1001234", client: clientHTTP(true)}
	m := &Message{Le: time.Now(), Gravite: alertes.Avertissement, Actives: []alertes.Alerte{{Regle: "R", Resume: "a<b"}}}
	if err := tg.Envoyer(ctx, m, Mise{}); err != nil {
		t.Fatal(err)
	}
	if chemin != "/bot"+jetonTest+"/sendMessage" || recu["parse_mode"] != "HTML" || recu["disable_notification"] != true ||
		!strings.Contains(recu["text"].(string), "a&lt;b") || recu["chat_id"] != "-1001234" {
		t.Fatalf("requête Telegram : %s %v", chemin, recu)
	}
	if err := tg.Verifier(ctx); err != nil || chemin != "/bot"+jetonTest+"/getMe" {
		t.Fatalf("getMe : %v %s", err, chemin)
	}
	// Serveur injoignable : l'erreur ne contient jamais le jeton.
	srv.Close()
	err := tg.Verifier(ctx)
	if err == nil || strings.Contains(err.Error(), jetonTest) || strings.Contains(err.Error(), "AAAA") {
		t.Fatalf("erreur avec le jeton : %v", err)
	}
	// Protection SSRF : adresse interne refusée sans reseau_prive.
	tg2 := &telegram{api: "http://127.0.0.1:1", jeton: jetonTest, chat: "1", client: clientHTTP(false)}
	if err := tg2.Verifier(ctx); err == nil || !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("SSRF : %v", err)
	}

	// ntfy : en-têtes, jeton, santé.
	var entetes http.Header
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entetes = r.Header.Clone()
		if r.URL.Path == "/v1/health" {
			_, _ = io.WriteString(w, `{"healthy":true}`)
		}
	}))
	defer srv2.Close()
	n := &ntfy{url: srv2.URL + "/mirador", jeton: "tk_secret", client: clientHTTP(true)}
	m.Gravite, m.Son = alertes.Critique, true
	if err := n.Envoyer(ctx, m, Mise{}); err != nil {
		t.Fatal(err)
	}
	if entetes.Get("Priority") != "urgent" || entetes.Get("Authorization") != "Bearer tk_secret" || entetes.Get("Title") == "" {
		t.Fatalf("en-têtes ntfy : %v", entetes)
	}
	if err := n.Verifier(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeat(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	var appels atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { appels.Add(1) }))
	defer srv.Close()
	horloge := time.Now()
	evalOK := horloge
	r := ReglagesDefaut()
	r.Canaux.ReseauPrive = true
	r.Canaux.Heartbeat = &Heartbeat{Actif: true, URL: "https://hetrix.example.org/x", Intervalle: alertes.Duree(time.Minute)}
	if err := EcrireReglages(ctx, db, r, cleTest, "test", horloge); err != nil {
		t.Fatal(err)
	}
	// L'URL de test est en http : on la pose chiffrée directement.
	r2, _ := LireReglages(ctx, db)
	r2.Canaux.Heartbeat.URL, _ = chiffrerTest(srv.URL)
	if err := EcrireReglages(ctx, db, r2, cleTest, "test", horloge); err != nil {
		t.Fatal(err)
	}
	p, err := NouveauPostier(ctx, OptionsPostier{DB: db, Cle: cleTest, Maintenant: func() time.Time { return horloge },
		DerniereEvaluationOK: func() time.Time { return evalOK }})
	if err != nil {
		t.Fatal(err)
	}
	p.Tour(ctx)
	horloge = horloge.Add(30 * time.Second)
	p.Tour(ctx) // pas encore l'intervalle
	if appels.Load() != 1 {
		t.Fatalf("%d appel(s), 1 attendu", appels.Load())
	}
	// Règles plus évaluées depuis 10 min : heartbeat suspendu.
	horloge = horloge.Add(10 * time.Minute)
	p.Tour(ctx)
	if _, _, e := p.Etat(); appels.Load() != 1 || !strings.Contains(e, "suspendu") {
		t.Fatalf("heartbeat non suspendu : %d %q", appels.Load(), e)
	}
	evalOK = horloge
	horloge = horloge.Add(time.Minute)
	p.Tour(ctx)
	if appels.Load() != 2 {
		t.Fatalf("heartbeat non repris : %d", appels.Load())
	}
}

func TestSecretsJamaisEnClair(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	r := ReglagesDefaut()
	r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234"}
	r.Canaux.Heartbeat = &Heartbeat{Actif: true, URL: "https://hetrix.example.org/secret-hb", Intervalle: alertes.Duree(time.Minute)}
	if err := EcrireReglages(ctx, db, r, cleTest, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	var tout string
	for _, q := range []string{`SELECT valeur FROM reglages WHERE cle = 'notifications'`,
		`SELECT group_concat(avant || apres) FROM audit`} {
		var v string
		if err := db.QueryRow(q).Scan(&v); err != nil {
			t.Fatal(err)
		}
		tout += v
	}
	if strings.Contains(tout, jetonTest) || strings.Contains(tout, "secret-hb") {
		t.Fatal("secret en clair en base ou dans l'audit")
	}
	lu, _ := LireReglages(ctx, db)
	b, _ := json.Marshal(lu.Masques())
	if !strings.Contains(string(b), `"jeton":"***"`) {
		t.Errorf("masques : %s", b)
	}
	// Mauvaise clé : les canaux ne se construisent pas (erreur claire).
	if _, err := fabriquer(&lu.Canaux, []byte("une autre clé de 32 octets......")); err == nil {
		t.Error("secret lu avec une mauvaise clé")
	}
}

func chiffrerTest(clair string) (string, error) {
	return secrets.Chiffrer(cleTest, usageHeartbeat, clair)
}

func TestAcquittementTelegram(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	var mu sync.Mutex
	var envoye map[string]any
	var reponses []string
	retires := 0
	majs := `[{"update_id": 10, "callback_query": {"id": "c1", "data": "ack:3", "from": {"id": 42, "username": "moi"},
	            "message": {"message_id": 7, "chat": {"id": -1001234}}}},
	          {"update_id": 11, "callback_query": {"id": "c2", "data": "ack:4", "from": {"id": 42},
	            "message": {"message_id": 8, "chat": {"id": -999}}}},
	          {"update_id": 12, "callback_query": {"id": "c3", "data": "ack:4", "from": {"id": 666},
	            "message": {"message_id": 8, "chat": {"id": -1001234}}}}]`
	lus := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		var corps map[string]any
		_ = json.Unmarshal(b, &corps)
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			envoye = corps
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			if lus == 0 && corps["allowed_updates"].([]any)[0] == "callback_query" {
				lus++
				_, _ = io.WriteString(w, `{"ok":true,"result":`+majs+`}`)
				return
			}
			if corps["offset"].(float64) != 13 {
				t.Errorf("offset %v, 13 attendu", corps["offset"])
			}
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			reponses = append(reponses, corps["text"].(string))
		case strings.HasSuffix(r.URL.Path, "/editMessageReplyMarkup"):
			retires++
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
	}))
	defer srv.Close()
	avant := APITelegram
	APITelegram = srv.URL
	defer func() { APITelegram = avant }()

	r := ReglagesDefaut()
	r.Mode = Actif
	r.Canaux.ReseauPrive = true // serveur de test sur 127.0.0.1
	r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234", Acquitter: true, Acquitteurs: []int64{42}}
	horloge := time.Now()
	if err := EcrireReglages(ctx, db, r, cleTest, "test", horloge); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{3, 4} {
		if _, err := db.Exec(`INSERT INTO alertes(id, regle, etiquettes, gravite, etat, debut) VALUES (?, 'X', '{}', 'critical', 'active', 0)`, id); err != nil {
			t.Fatal(err)
		}
	}
	p, err := NouveauPostier(ctx, OptionsPostier{DB: db, Cle: cleTest, Maintenant: func() time.Time { return horloge },
		DerniereEvaluationOK: func() time.Time { return horloge }})
	if err != nil {
		t.Fatal(err)
	}
	// Le message porte un bouton par alerte.
	a := alertes.Alerte{ID: 3, Regle: "CpuEleve", Gravite: alertes.Critique, Etat: alertes.Active}
	p.Envoyer(ctx, &Message{Le: horloge, Gravite: alertes.Critique, Type: TypeAlerte, Actives: []alertes.Alerte{a}}, nil)
	mu.Lock()
	clavier, _ := json.Marshal(envoye["reply_markup"])
	mu.Unlock()
	if !strings.Contains(string(clavier), `"callback_data":"ack:3"`) {
		t.Fatalf("bouton absent : %s", clavier)
	}
	// Clics : 3 acquittée par la personne autorisée ; 4 venue d'un autre
	// groupe (ignorée) ou d'une personne non autorisée (refusée).
	p.Tour(ctx)
	horloge = horloge.Add(RythmeClics)
	p.Tour(ctx)
	var par string
	var ack4 int64
	_ = db.QueryRow(`SELECT acquittee_par FROM alertes WHERE id = 3`).Scan(&par)
	_ = db.QueryRow(`SELECT acquittee_le FROM alertes WHERE id = 4`).Scan(&ack4)
	mu.Lock()
	defer mu.Unlock()
	if par != "telegram:42 (@moi)" || ack4 != 0 || retires != 1 || len(reponses) != 2 ||
		!strings.Contains(reponses[0], "n° 3 acquittée") || !strings.Contains(reponses[1], "pas autorisé") {
		t.Fatalf("acquittements : par=%q ack4=%d retirés=%d réponses=%v", par, ack4, retires, reponses)
	}
	// Bouton demandé sans identifiant numérique du groupe : refusé.
	r.Canaux.Telegram.Chat = "@mongroupe"
	r.Canaux.Telegram.Jeton = "***"
	if err := EcrireReglages(ctx, db, r, cleTest, "test", horloge); err == nil {
		t.Error("acquitter avec @nom accepté")
	}
}

// Message de sécurité de la console : envoyé en mode actif, seulement noté
// dans l'historique en mode ombre.
func TestPrevenir(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{Actif, Ombre} {
		db := baseTest(t)
		r := ReglagesDefaut()
		r.Mode = mode
		r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234"}
		if err := EcrireReglages(ctx, db, r, cleTest, "test", time.Now()); err != nil {
			t.Fatal(err)
		}
		tg := &canalSimule{nom: NomTelegram}
		p, err := NouveauPostier(ctx, OptionsPostier{DB: db, Cle: cleTest, DerniereEvaluationOK: time.Now,
			Fabrique: func(*Canaux, []byte) (map[string]Canal, error) {
				return map[string]Canal{NomTelegram: tg}, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		p.Prevenir(ctx, "securite=2fa|admin", "code 2FA faux pour admin")
		recus := tg.vider()
		var notes int
		_ = db.QueryRow(`SELECT COUNT(*) FROM envois WHERE type = 'securite'`).Scan(&notes)
		if mode == Actif && (len(recus) != 1 || !contient(recus, "code 2FA faux")) {
			t.Errorf("mode actif : %d message(s) %v", len(recus), recus)
		}
		if mode == Ombre && len(recus) != 0 {
			t.Errorf("mode ombre : message parti %v", recus)
		}
		if notes != 1 {
			t.Errorf("mode %s : %d ligne(s) dans l'historique, 1 attendue", mode, notes)
		}
	}
}
