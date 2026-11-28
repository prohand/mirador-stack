// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

// messageAvecLogs : une alerte nouvelle et ses extraits de logs.
func messageAvecLogs(lignes ...string) *Message {
	t0 := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	a := alerte(7, "ServiceArrete", alertes.Critique, "nas", t0.Add(-time.Hour), "name", "ssh.service")
	return &Message{Le: t0, Type: TypeAlerte, Gravite: alertes.Critique, Nouvelles: 1, Actives: []alertes.Alerte{a},
		Extraits: map[uint64][]string{a.Etiquettes.Hash(): lignes}}
}

// Revue du 28/11 (M5) : ni mention ni faux lien venus des logs, sur les
// formats de messagerie du webhook.
func TestWebhookSansMentions(t *testing.T) {
	m := messageAvecLogs("Invalid user @everyone from 203.0.113.9", "<!channel> [Connexion mirador](https://piege.example/) ``` fin",
		"<https://piege.example/|Connexion mirador> & co")
	o := Mise{Lien: "https://mirador.example.org"}

	// Discord : aucune mention permise, logs en bloc de code fermé.
	b, err := corpsWebhook(FormatDiscord, m, o)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Content  string `json:"content"`
		Mentions struct {
			Parse *[]string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.Mentions.Parse == nil || len(*d.Mentions.Parse) != 0 {
		t.Errorf("discord : allowed_mentions absent : %s", b)
	}
	if strings.Count(d.Content, "```") != 2 || !strings.Contains(d.Content, "```\nInvalid user @everyone") {
		t.Errorf("discord : logs hors d'un bloc de code (ou ``` des logs non neutralisés) :\n%s", d.Content)
	}

	// Slack / Mattermost : < > & échappés, mentions coupées, bloc de code.
	b, err = corpsWebhook(FormatSlack, m, o)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for _, interdit := range []string{"<!channel>", "<https://piege", "@everyone", " & co"} {
		if strings.Contains(s.Text, interdit) {
			t.Errorf("slack : %q présent :\n%s", interdit, s.Text)
		}
	}
	for _, attendu := range []string{"&lt;!channel&gt;", "&lt;https://piege.example/|Connexion mirador&gt; &amp; co", "@​everyone"} {
		if !strings.Contains(s.Text, attendu) {
			t.Errorf("slack : %q absent :\n%s", attendu, s.Text)
		}
	}
	if strings.Count(s.Text, "```") != 2 {
		t.Errorf("slack : bloc de code :\n%s", s.Text)
	}

	// Matrix : m.mentions vide, « @room » coupé.
	m = messageAvecLogs("@room réveillez-vous")
	b, err = corpsWebhook(FormatMatrix, m, o)
	if err != nil {
		t.Fatal(err)
	}
	var x map[string]any
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	if _, ok := x["m.mentions"].(map[string]any); !ok || strings.Contains(x["body"].(string), "@room") ||
		strings.Contains(x["formatted_body"].(string), "@room") {
		t.Errorf("matrix : %s", b)
	}
}

// Revue du 28/11 (B6) : des logs pleins de guillemets (5 octets chacun une
// fois échappés) ne font plus couper le message au milieu du <pre>.
func TestCouperSansBaliseOuverte(t *testing.T) {
	guillemets := strings.Repeat(`"`, 159)
	var lignes []string
	for i := 0; i < ExtraitLignes; i++ {
		lignes = append(lignes, guillemets)
	}
	m := messageAvecLogs(lignes...)
	// Trois alertes avec leurs logs : bien plus que 4 000 octets sans borne.
	for i := 0; i < 2; i++ {
		a := alerte(int64(8+i), "ServiceArrete", alertes.Critique, "nas", m.Le.Add(-time.Hour), "name", "x"+string(rune('a'+i))+".service")
		m.Actives = append(m.Actives, a)
		m.Extraits[a.Etiquettes.Hash()] = lignes
	}
	m.Nouvelles = 3
	h := Texte(m, Mise{HTML: true, Max: 4000})
	if len(h) > 4000 {
		t.Fatalf("%d octets", len(h))
	}
	if strings.Count(h, "<pre>") != strings.Count(h, "</pre>") || strings.Count(h, "<b>") != strings.Count(h, "</b>") {
		t.Errorf("balise laissée ouverte :\n%s", h)
	}
	// Chaque bloc de logs est borné après échappement.
	for _, bloc := range strings.Split(h, "<pre>")[1:] {
		if i := strings.Index(bloc, "</pre>"); i < 0 || i > extraitOctets {
			t.Errorf("bloc de logs de %d octets", i)
		}
	}

	// Coupe brute au milieu d'un bloc : on coupe avant le <pre>.
	s := "<b>Titre</b>\nligne\n<pre>" + strings.Repeat("a&#34;\n", 200) + "</pre>\nfin"
	c := couperTexte(s, 300, true, false)
	if strings.Contains(c, "<pre>") || !strings.Contains(c, "<b>Titre</b>") || !strings.HasSuffix(c, "voir l'interface)") {
		t.Errorf("coupe HTML : %q", c)
	}
	// Balise ou entité coupée en deux : retirée.
	for _, cas := range []string{"début <pr", "début &#3", "début <b>gras"} {
		if got := avantBaliseOuverte(cas); got != "début " {
			t.Errorf("avantBaliseOuverte(%q) = %q", cas, got)
		}
	}
	// Markdown : jamais au milieu d'un bloc ```.
	md := "titre\n```\n" + strings.Repeat("ligne de log\n", 100) + "```\nfin"
	if c := couperTexte(md, 200, false, true); strings.Count(c, "```")%2 != 0 {
		t.Errorf("coupe Markdown : %q", c)
	}
}

// Revue du 28/11 (B6) : si Telegram refuse le HTML (« can't parse
// entities »), le message est renvoyé une fois en texte brut.
func TestTelegramRepliTexteBrut(t *testing.T) {
	var mu sync.Mutex
	var corps []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var c map[string]any
		_ = json.Unmarshal(b, &c)
		mu.Lock()
		corps = append(corps, c)
		mu.Unlock()
		if _, html := c["parse_mode"]; html {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: can't parse entities: unclosed start tag at byte offset 3990"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	tg := &telegram{api: srv.URL, jeton: jetonTest, chat: "-1001234", client: clientHTTP(true)}
	if err := tg.Envoyer(context.Background(), messageAvecLogs(`"guillemets"`), Mise{}); err != nil {
		t.Fatalf("repli : %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(corps) != 2 {
		t.Fatalf("%d envois (2 attendus : HTML puis texte brut)", len(corps))
	}
	if txt, _ := corps[1]["text"].(string); strings.Contains(txt, "<pre>") || !strings.Contains(txt, `"guillemets"`) {
		t.Errorf("texte brut : %q", txt)
	}
	// Autre erreur 400 : pas de nouvel essai.
	if miseEnFormeRefusee(&erreurTelegram{code: 400, description: "Bad Request: chat not found"}) {
		t.Error("« chat not found » pris pour une erreur de mise en forme")
	}
}

// Revue du 28/11 (B7) : liste commune ; CGNAT, 0.0.0.0/8, NAT64 refusés
// au moment de la connexion.
func TestNumeroteurListeCommune(t *testing.T) {
	d := numeroteur(false)
	for _, a := range []string{"100.64.0.1:443", "0.0.0.1:443", "198.18.0.1:443", "[64:ff9b::7f00:1]:443", "[::ffff:127.0.0.1]:443"} {
		if err := d.Control("tcp", a, nil); err == nil {
			t.Errorf("%s permise", a)
		}
	}
	if err := d.Control("tcp", net.JoinHostPort("8.8.8.8", "443"), nil); err != nil {
		t.Errorf("adresse publique refusée : %v", err)
	}
	if numeroteur(true).Control != nil {
		t.Error("réseau privé permis : pas de contrôle attendu")
	}
}
