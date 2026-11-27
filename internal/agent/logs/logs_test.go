// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"

	"mirador-stack/internal/agent/journal"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/protocole"
)

var tousModeles = []string{"mots_de_passe", "tokens", "cles_api", "autorisation", "cartes", "emails"}

func TestMasquage(t *testing.T) {
	m, err := NewMasqueur(tousModeles, []string{`IBAN [A-Z]{2}\d{2}[A-Z0-9 ]{10,30}`})
	if err != nil {
		t.Fatal(err)
	}
	cas := map[string]string{
		"login ok user=bob password=Sup3rS3cret! from 10.0.0.1": "login ok user=bob password=*** from 10.0.0.1",
		`{"passwd": "abc123", "user":"x"}`:                      `{"passwd": "***", "user":"x"}`,
		"GET /api?token=eyJhbGciOi.abc.def&x=1":                 "GET /api?token=***&x=1",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.e30.abc":    "Authorization: Bearer ***",
		"header authorization=Basic dXNlcjpwYXNz":               "header authorization=Basic ***",
		"api_key=AKIA1234567890ABCD done":                       "api_key=*** done",
		"paiement carte 4111 1111 1111 1111 accepté":            "paiement carte *** accepté",
		"commande 1234567890123456 (pas une carte)":             "commande 1234567890123456 (pas une carte)",
		"mail envoyé à jean.dupont@exemple.fr":                  "mail envoyé à ***",
		"virement IBAN FR76 3000 6000 0112 3456 7890 189":       "virement ***",
		"ligne sans rien de sensible":                           "ligne sans rien de sensible",
	}
	for entree, attendu := range cas {
		if obtenu := m.Masquer(entree); obtenu != attendu {
			t.Errorf("Masquer(%q)\n  = %q\n  attendu %q", entree, obtenu, attendu)
		}
	}
}

func TestMasquageLimites(t *testing.T) {
	if _, err := NewMasqueur([]string{"inconnu"}, nil); err == nil {
		t.Error("modèle inconnu accepté")
	}
	if _, err := NewMasqueur(nil, []string{"("}); err == nil {
		t.Error("expression invalide acceptée")
	}
	if _, err := NewMasqueur(nil, []string{strings.Repeat("a", LongueurMaxRe+1)}); err == nil {
		t.Error("expression trop longue acceptée")
	}
	trop := make([]string, MaxPerso+1)
	for i := range trop {
		trop[i] = "x"
	}
	if _, err := NewMasqueur(nil, trop); err == nil {
		t.Error("trop d'expressions acceptées")
	}
	// Ligne énorme : temps borné (moteur linéaire + coupure à 16 Kio).
	m, _ := NewMasqueur(tousModeles, nil)
	debut := time.Now()
	m.Masquer(strings.Repeat("password=x ", 200_000))
	if time.Since(debut) > time.Second {
		t.Error("masquage trop lent sur une ligne énorme")
	}
}

func TestLuhn(t *testing.T) {
	for _, ok := range []string{"4111111111111111", "5500 0000 0000 0004", "3782-822463-10005"} {
		if !luhn(ok) {
			t.Errorf("%s devrait être valide", ok)
		}
	}
	for _, ko := range []string{"4111111111111112", "123", "12345678901234567890"} {
		if luhn(ko) {
			t.Errorf("%s devrait être invalide", ko)
		}
	}
}

// relireLot décompresse un lot et renvoie ses lignes JSON décodées.
func relireLot(t *testing.T, gz []byte) []map[string]string {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	brut, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var lignes []map[string]string
	sc := bufio.NewScanner(bytes.NewReader(brut))
	for sc.Scan() {
		if !json.Valid(sc.Bytes()) {
			t.Fatalf("ligne JSON invalide : %s", sc.Text())
		}
		var m map[string]string
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		lignes = append(lignes, m)
	}
	return lignes
}

func TestLotJSON(t *testing.T) {
	l := NewLot("srv-test-mirador", "journal", "bureau", "vm", 1<<20)
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 123456000, time.UTC)
	l.Ajouter(t0, "ligne \"guillemets\" \\ tab\t retour\n couleur \x1b[31m invalide \xff fin", Champs{{"unit", "ssh.service"}, {"level", "err"}, {"ident", "sshd"}})
	l.Ajouter(t0, "noyau", Champs{{"unit", ""}, {"level", "warning"}, {"ident", "kernel"}})
	gz, n := l.Vider()
	if n != 2 {
		t.Fatalf("2 lignes attendues, %d", n)
	}
	lignes := relireLot(t, gz)
	p := lignes[0]
	if p["_time"] != "2026-11-08T10:00:00.123456Z" || p["host"] != "srv-test-mirador" || p["job"] != "journal" ||
		p["site"] != "bureau" || p["env"] != "vm" || p["unit"] != "ssh.service" || p["level"] != "err" || p["ident"] != "sshd" {
		t.Errorf("champs incorrects : %v", p)
	}
	if !strings.Contains(p["_msg"], `"guillemets"`) || !strings.Contains(p["_msg"], "�") {
		t.Errorf("message mal échappé : %q", p["_msg"])
	}
	if _, existe := lignes[1]["unit"]; existe {
		t.Error("une étiquette vide ne doit pas être envoyée")
	}
	if gz, _ := l.Vider(); gz != nil {
		t.Error("le lot doit être vide après Vider")
	}
}

func entree(msg string, prio int, unite, ident, transport string) *journal.Entree {
	return &journal.Entree{Temps: time.Now(), Message: msg, Priorite: prio, Unite: unite, Ident: ident, Transport: transport}
}

func TestTraitement(t *testing.T) {
	lot := NewLot("h", "journal", "s", "e", 1<<20)
	masque, _ := NewMasqueur([]string{"mots_de_passe"}, nil)
	tr := NewTraitement(Options{Bruit: `(?i)session (opened|closed) for user`, Debit: 1000, Rafale: 1000, Masque: masque}, lot, nil)

	tr.Traiter(entree("pam_unix(sshd:session): session opened for user bob", 6, "ssh.service", "sshd", "syslog"))
	tr.Traiter(entree("Failed password for root from 1.2.3.4 port 22 ssh2", 5, "ssh.service", "sshd", "syslog"))
	tr.Traiter(entree("Invalid user admin from 1.2.3.4", 5, "ssh.service", "sshd", "syslog"))
	tr.Traiter(entree("Out of memory: Killed process 1234 (java) total-vm:123kB", 3, "", "kernel", "kernel"))
	tr.Traiter(entree("blk_update_request: I/O error, dev sda, sector 123", 3, "", "kernel", "kernel"))
	tr.Traiter(entree("db password=secret", 6, "app.service", "app", "stdout"))

	b := metrics.NewBatch(64, nil)
	b.Reset(0)
	tr.Metriques(b)
	got := map[string]float64{}
	for i := range b.Samples {
		s := &b.Samples[i]
		k := s.Name
		for _, l := range s.Labels[:s.N] {
			k += "|" + l.Value
		}
		got[k] = s.Value
	}
	for k, v := range map[string]float64{
		"mirador_logs_dropped_total|bruit":      1,
		"mirador_logs_ssh_auth_failures_total":  2,
		"mirador_logs_kernel_disk_errors_total": 1,
		"mirador_logs_oom_kills_total|java":     1,
		"mirador_logs_lines_total|err":          2,
		"mirador_logs_masked_total":             1,
	} {
		if got[k] != v {
			t.Errorf("%s = %v, attendu %v (tout : %v)", k, got[k], v, got)
		}
	}
	gz, n := lot.Vider()
	if n != 5 {
		t.Errorf("5 lignes gardées attendues (1 bruit jetée), %d", n)
	}
	for _, l := range relireLot(t, gz) {
		if strings.Contains(l["_msg"], "secret") {
			t.Errorf("mot de passe non masqué : %s", l["_msg"])
		}
	}
}

func TestLimiteDebit(t *testing.T) {
	lot := NewLot("h", "journal", "s", "e", 1<<20)
	tr := NewTraitement(Options{Debit: 10, Rafale: 20}, lot, nil)
	t0 := time.Now()
	maintenant := t0
	tr.horloge = func() time.Time { return maintenant }
	for i := 0; i < 100; i++ { // 100 lignes dans la même seconde
		e := entree("x", 6, "", "", "")
		e.Temps = t0
		tr.Traiter(e)
	}
	if _, n := lot.Vider(); n != 20 {
		t.Errorf("rafale de 20 attendue, %d lignes gardées", n)
	}
	e := entree("x", 6, "", "", "")
	e.Temps = t0.Add(time.Second) // 1 s plus tard : 10 jetons de plus
	maintenant = t0.Add(time.Second)
	for i := 0; i < 30; i++ {
		tr.Traiter(e)
	}
	if _, n := lot.Vider(); n != 10 {
		t.Errorf("10 lignes attendues après 1 s, %d", n)
	}
}

// Relecture du 21/11 : une ligne datée dans le futur ne bloque plus la
// recharge (horloge réelle, pas l'heure des lignes).
func TestLimiteDebitLigneFuture(t *testing.T) {
	lot := NewLot("h", "journal", "s", "e", 1<<20)
	tr := NewTraitement(Options{Debit: 10, Rafale: 20}, lot, nil)
	maintenant := time.Now()
	tr.horloge = func() time.Time { return maintenant }
	futur := entree("x", 6, "", "", "")
	futur.Temps = maintenant.Add(time.Hour)
	tr.Traiter(futur)
	for s := 0; s < 30; s++ { // 30 s à 10 lignes/s
		maintenant = maintenant.Add(time.Second)
		for i := 0; i < 10; i++ {
			e := entree("x", 6, "", "", "")
			e.Temps = maintenant
			tr.Traiter(e)
		}
	}
	if _, n := lot.Vider(); n < 300 {
		t.Errorf("lignes gardées : %d sur 301", n)
	}
}

func TestOOMNombreBorne(t *testing.T) {
	tr := NewTraitement(Options{}, NewLot("h", "journal", "s", "e", 1<<20), nil)
	for i := 0; i < maxProcessusOOM+20; i++ {
		tr.Traiter(entree("Out of memory: Killed process 1 (proc"+strings.Repeat("x", i%60)+string(rune('a'+i%26))+")", 3, "", "kernel", "kernel"))
	}
	if len(tr.oom) > maxProcessusOOM+1 {
		t.Errorf("nombre de processus OOM non borné : %d", len(tr.oom))
	}
}

func BenchmarkTraitement(b *testing.B) {
	lot := NewLot("h", "journal", "s", "e", 1<<30)
	masque, _ := NewMasqueur([]string{"mots_de_passe", "tokens", "cles_api", "autorisation", "cartes"}, nil)
	tr := NewTraitement(Options{Bruit: `(?i)(session (opened|closed) for user|pam_unix\(cron)`, Debit: 1e9, Rafale: 1e9, Masque: masque}, lot, nil)
	e := entree("nginx: 10.0.0.1 - - [08/Nov/2026:10:00:00 +0100] \"GET /index.html HTTP/1.1\" 200 512 \"-\" \"Mozilla/5.0\"", 6, "nginx.service", "nginx", "stdout")
	b.ReportAllocs()
	for b.Loop() {
		tr.Traiter(e)
	}
}

func FuzzMasquerEtJSON(f *testing.F) {
	f.Add("password=abc token=def 4111 1111 1111 1111 a@b.fr")
	f.Add("\xff\xfe\x00\"\\")
	m, _ := NewMasqueur(tousModeles, nil)
	f.Fuzz(func(t *testing.T, s string) {
		out := m.Masquer(s)
		b := ajouterChaine(nil, out)
		if !json.Valid(b) {
			t.Fatalf("JSON invalide pour %q", s)
		}
	})
}

// Connexions SSH réussies (demande du 27/11) : signalées au serveur, une
// fois par adresse et utilisateur toutes les 10 min ; rien sans fonction.
func TestConnexionsSSH(t *testing.T) {
	tr := NewTraitement(Options{Debit: 1000, Rafale: 1000}, NewLot("h", "journal", "s", "e", 1<<20), nil)
	h := time.Date(2026, 11, 27, 21, 0, 0, 0, time.UTC)
	tr.horloge = func() time.Time { return h }
	ligne := "Accepted publickey for admin from 198.51.100.23 port 52344 ssh2: ED25519 SHA256:abc"
	tr.Traiter(entree(ligne, 6, "ssh.service", "sshd", "syslog")) // sans fonction : rien
	var vus []protocole.EvenementAgent
	tr.SignalerConnexions(func(e protocole.EvenementAgent) { vus = append(vus, e) })
	tr.Traiter(entree(ligne, 6, "ssh.service", "sshd", "syslog"))
	tr.Traiter(entree(ligne, 6, "ssh.service", "sshd", "syslog")) // même minute : une fois
	tr.Traiter(entree("Accepted password for bob from 2001:db8::7 port 1 ssh2", 6, "ssh.service", "sshd", "syslog"))
	tr.Traiter(entree("Failed password for root from 203.0.113.9 port 22 ssh2", 5, "ssh.service", "sshd", "syslog"))
	tr.Traiter(entree(ligne, 6, "app.service", "app", "stdout")) // pas sshd
	if len(vus) != 2 || vus[0].Type != protocole.EvenementSSH || vus[0].Objet != "198.51.100.23" || vus[0].Texte != "admin · publickey" ||
		vus[1].Objet != "2001:db8::7" || vus[1].Texte != "bob · password" {
		t.Fatalf("connexions : %+v", vus)
	}
	for i := range vus {
		if err := vus[i].Valider(); err != nil {
			t.Errorf("événement refusé : %v", err)
		}
	}
	h = h.Add(11 * time.Minute)
	tr.Traiter(entree(ligne, 6, "ssh.service", "sshd", "syslog"))
	if len(vus) != 3 {
		t.Errorf("après 10 min : %d", len(vus))
	}
}
