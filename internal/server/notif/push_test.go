// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/webpush"
)

// servicePushSimule : faux service de push (aucune requête ne sort). Le
// code de réponse dépend de l'adresse de l'abonnement.
type servicePushSimule struct {
	mu     sync.Mutex
	codes  map[string]int    // chemin → code HTTP (201 par défaut)
	recus  map[string][]byte // chemin → dernier corps chiffré
	appels int
}

func (s *servicePushSimule) RoundTrip(r *http.Request) (*http.Response, error) {
	corps, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appels++
	s.recus[r.URL.Path] = corps
	code := s.codes[r.URL.Path]
	if code == 0 {
		code = http.StatusCreated
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func installerServicePush(t *testing.T) *servicePushSimule {
	t.Helper()
	s := &servicePushSimule{codes: map[string]int{}, recus: map[string][]byte{}}
	avant := ClientPush
	ClientPush = func() *http.Client { return &http.Client{Transport: s} }
	t.Cleanup(func() { ClientPush = avant })
	return s
}

type appareilTest struct {
	chemin string
	cle    *ecdh.PrivateKey
	a      webpush.Abonnement
}

func nouvelAppareilTest(t *testing.T, chemin string) appareilTest {
	t.Helper()
	cle, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return appareilTest{chemin: chemin, cle: cle, a: webpush.Abonnement{Adresse: "https://fcm.googleapis.com" + chemin, P256dh: cle.PublicKey().Bytes(), Auth: auth}}
}

func compteTest(t *testing.T, db *sql.DB, nom string, desactive int) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO utilisateurs(nom, role, mot_de_passe, desactive, cree_le) VALUES (?, 'admin', 'x', ?, 1)`, nom, desactive)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func nombreAbonnements(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM abonnements_push`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Canal « application » : copie vers les appareils, abonnement expiré
// effacé, compte désactivé ignoré, pastille, bascule si aucun appareil.
func TestCanalApplication(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	service := installerServicePush(t)
	t0 := time.Unix(1_800_000_000, 0)
	lea, parti := compteTest(t, db, "lea", 0), compteTest(t, db, "parti", 1)
	tel, vieux, autre := nouvelAppareilTest(t, "/fcm/send/tel"), nouvelAppareilTest(t, "/fcm/send/vieux"), nouvelAppareilTest(t, "/fcm/send/parti")
	for _, x := range []struct {
		compte int64
		ap     appareilTest
	}{{lea, tel}, {lea, vieux}, {parti, autre}} {
		if err := AbonnerPush(ctx, db, x.compte, x.ap.a, "https://mirador.exemple.fr", "Chrome sur Android", t0); err != nil {
			t.Fatal(err)
		}
	}
	service.codes[vieux.chemin] = http.StatusGone // application désinstallée
	// Deux alertes en cours en base, dont une prise en charge : pastille = 1.
	for _, acq := range []int{0, 5} {
		if _, err := db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, acquittee_le) VALUES ('X', '{}', 'critical', 'active', 1, ?)`, acq); err != nil {
			t.Fatal(err)
		}
	}

	r := ReglagesDefaut()
	r.Mode = Actif
	r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234"}
	r.Canaux.Application = &Application{Actif: true, Copie: true}
	if err := EcrireReglages(ctx, db, r, cleTest, "test", t0); err != nil {
		t.Fatal(err)
	}
	tg := &canalSimule{nom: NomTelegram}
	p, err := NouveauPostier(ctx, OptionsPostier{DB: db, Cle: cleTest, DerniereEvaluationOK: time.Now, Maintenant: func() time.Time { return t0 },
		Fabrique: func(*Canaux, []byte) (map[string]Canal, error) { return map[string]Canal{NomTelegram: tg}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	alerte := alertes.Alerte{ID: 42, Regle: "AgentMuet", Gravite: alertes.Critique, Resume: "pve1 ne remonte plus de métriques",
		Etiquettes: labels.FromStrings("alertname", "AgentMuet", "instance", "pve1")}
	p.Envoyer(ctx, &Message{Le: t0, Groupe: "g1", Gravite: alertes.Critique, Actives: []alertes.Alerte{alerte}, Nouvelles: 1, Son: true}, nil)

	// Telegram (principal) a le message ; les appareils de lea ont la copie.
	if n := len(tg.vider()); n != 1 {
		t.Fatalf("telegram : %d message(s)", n)
	}
	if service.recus[autre.chemin] != nil {
		t.Error("appareil d'un compte désactivé servi")
	}
	clair, err := webpush.Dechiffrer(service.recus[tel.chemin], tel.cle, tel.a.Auth)
	if err != nil {
		t.Fatalf("notification du téléphone illisible : %v", err)
	}
	var c ChargePush
	if err := json.Unmarshal(clair, &c); err != nil {
		t.Fatal(err)
	}
	if c.Titre != "Agent muet · pve1" || c.Lien != "/alertes/42" || c.Pastille != 1 || !c.Son || c.Groupe != "g1" ||
		!strings.Contains(c.Corps, "pve1 ne remonte plus") || strings.Contains(c.Corps, "http") {
		t.Fatalf("contenu de la notification : %+v", c)
	}
	// Abonnement expiré effacé, les deux autres gardés.
	if n := nombreAbonnements(t, db); n != 2 {
		t.Fatalf("%d abonnement(s) après un 410, 2 attendus", n)
	}
	// La copie est notée à part et ne met aucun canal en panne.
	var copies int
	_ = db.QueryRow(`SELECT count(*) FROM envois WHERE canal = ? AND statut = 'ok'`, NomApplication).Scan(&copies)
	if copies != 1 {
		t.Errorf("%d copie(s) notée(s) dans l'historique", copies)
	}

	// Copie décochée : plus rien vers les appareils.
	appels := service.appels
	r.Canaux.Application.Copie = false
	if err := EcrireReglages(ctx, db, r, cleTest, "test", t0); err != nil {
		t.Fatal(err)
	}
	p.Envoyer(ctx, &Message{Le: t0, Groupe: "g2", Gravite: alertes.Info, Actives: []alertes.Alerte{{Regle: "Y", Gravite: alertes.Info}}}, nil)
	if service.appels != appels || len(tg.vider()) != 1 {
		t.Errorf("copie décochée : %d appel(s) en plus", service.appels-appels)
	}

	// Application en canal principal, Telegram en secours : le téléphone
	// reçoit, Telegram non ; sans appareil, bascule sur Telegram.
	r.Canaux.Principal, r.Canaux.Secours = NomApplication, NomTelegram
	if err := EcrireReglages(ctx, db, r, cleTest, "test", t0); err != nil {
		t.Fatal(err)
	}
	p.Envoyer(ctx, &Message{Le: t0, Groupe: "g3", Gravite: alertes.Info, Systeme: "essai"}, nil)
	if service.appels != appels+1 || len(tg.vider()) != 0 {
		t.Fatalf("application principale : %d appel(s), telegram servi à tort", service.appels-appels)
	}
	if _, err := db.Exec(`DELETE FROM abonnements_push`); err != nil {
		t.Fatal(err)
	}
	p.Envoyer(ctx, &Message{Le: t0, Groupe: "g4", Gravite: alertes.Info, Systeme: "essai 2"}, nil)
	if msgs := tg.vider(); len(msgs) != 1 || !contient(msgs, "essai 2") {
		t.Fatalf("sans appareil, pas de bascule sur le secours : %v", msgs)
	}
}

func TestAbonnementsPush(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	t0 := time.Unix(1_800_000_000, 0)
	lea, tom := compteTest(t, db, "lea", 0), compteTest(t, db, "tom", 0)
	ap := nouvelAppareilTest(t, "/fcm/send/commun")
	if err := AbonnerPush(ctx, db, lea, ap.a, "https://mirador.exemple.fr", "iPhone", t0); err != nil {
		t.Fatal(err)
	}
	// Même appareil, autre compte : il change de compte (un seul abonnement).
	if err := AbonnerPush(ctx, db, tom, ap.a, "https://mirador.exemple.fr", "iPhone", t0); err != nil {
		t.Fatal(err)
	}
	l, _ := ListerAppareilsPush(ctx, db, lea)
	m, _ := ListerAppareilsPush(ctx, db, tom)
	if len(l) != 0 || len(m) != 1 || m[0].Appareil != "iPhone" || m[0].Service != "fcm.googleapis.com" || len(m[0].Empreinte) != 16 {
		t.Fatalf("lea %d, tom %+v", len(l), m)
	}
	// Retirer : seulement ses propres appareils.
	if _, err := RetirerAppareilPush(ctx, db, lea, m[0].ID); err == nil {
		t.Error("lea a retiré l'appareil de tom")
	}
	// Refusés : service inconnu, interface sans HTTPS.
	mauvais := ap.a
	mauvais.Adresse = "https://127.0.0.1/x"
	if err := AbonnerPush(ctx, db, lea, mauvais, "https://mirador.exemple.fr", "x", t0); err == nil {
		t.Error("adresse interne acceptée")
	}
	if err := AbonnerPush(ctx, db, lea, ap.a, "http://mirador.exemple.fr", "x", t0); err == nil {
		t.Error("interface sans HTTPS acceptée")
	}
	// Dix appareils au plus par compte.
	for i := 0; i < MaxAppareilsPush; i++ {
		if err := AbonnerPush(ctx, db, lea, nouvelAppareilTest(t, fmt.Sprintf("/fcm/send/l%d", i)).a, "https://mirador.exemple.fr", "x", t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := AbonnerPush(ctx, db, lea, nouvelAppareilTest(t, "/fcm/send/de-trop").a, "https://mirador.exemple.fr", "x", t0); err == nil {
		t.Error("onzième appareil accepté")
	}
	// Compte supprimé : ses abonnements partent avec lui.
	if _, err := db.Exec(`DELETE FROM utilisateurs WHERE id = ?`, lea); err != nil {
		t.Fatal(err)
	}
	if n := nombreAbonnements(t, db); n != 1 {
		t.Errorf("%d abonnement(s) après la suppression du compte, 1 attendu", n)
	}
}

func TestClePush(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	t0 := time.Unix(1_800_000_000, 0)
	k1, pub1, err := ClePush(ctx, db, cleTest, t0)
	if err != nil {
		t.Fatal(err)
	}
	k2, pub2, err := ClePush(ctx, db, cleTest, t0)
	if err != nil || pub1 != pub2 || !k1.Equal(k2) {
		t.Fatalf("clé différente au second appel : %v", err)
	}
	// En base : clé privée chiffrée seulement.
	var v string
	_ = db.QueryRow(`SELECT valeur FROM reglages WHERE cle = ?`, cleVAPID).Scan(&v)
	var k clesVAPID
	if json.Unmarshal([]byte(v), &k) != nil || !secrets.EstChiffre(k.Prive) || k.Publique != pub1 {
		t.Fatalf("clé en base : %s", v)
	}
	// Autre clé des secrets : erreur claire, pas de nouvelle clé créée en silence.
	autre := append([]byte{}, cleTest...)
	autre[0] ^= 1
	if _, _, err := ClePush(ctx, db, autre, t0); err == nil {
		t.Error("clé lue avec une autre clé des secrets")
	}
}

func TestChargeDe(t *testing.T) {
	o := Mise{Fuseau: time.UTC, Lien: "https://mirador.exemple.fr"}
	if c := chargeDe(&Message{Systeme: "🔑 connexion depuis une nouvelle adresse", Groupe: "s"}, o, 0); c.Lien != "/" || c.Titre != "mirador" ||
		!strings.Contains(c.Corps, "nouvelle adresse") {
		t.Errorf("message système : %+v", c)
	}
	deux := []alertes.Alerte{{ID: 1, Regle: "CpuEleve", Etiquettes: labels.FromStrings("instance", "a")}, {ID: 2, Regle: "CpuEleve", Etiquettes: labels.FromStrings("instance", "b")}}
	if c := chargeDe(&Message{Actives: deux, Gravite: alertes.Avertissement}, o, 2); c.Lien != "/alertes" || c.Titre != "CPU élevé · a (+1)" {
		t.Errorf("deux alertes : %+v", c)
	}
	if c := chargeDe(&Message{Resolues: deux[:1]}, o, 0); c.Titre != "Résolu : CPU élevé · a" || c.Pastille != 0 {
		t.Errorf("résolution : %+v", c)
	}
	long := &Message{Systeme: strings.Repeat("très long ", 400)}
	c := chargeDe(long, o, 0)
	brut, _ := json.Marshal(c)
	if len([]rune(c.Corps)) > corpsPushMax || len(brut) > webpush.TailleMaxMessage {
		t.Errorf("corps de %d signes, notification de %d octets", len([]rune(c.Corps)), len(brut))
	}
	// Pire cas : 600 signes de 4 octets et un titre long restent sous la limite.
	pire := chargeDe(&Message{Systeme: strings.Repeat("😀", 2000), Groupe: strings.Repeat("g", 200)}, o, 99)
	if brut, _ = json.Marshal(pire); len(brut) > webpush.TailleMaxMessage {
		t.Errorf("pire cas : notification de %d octets", len(brut))
	}
}
