// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
)

var (
	cleTest   = []byte("0123456789abcdef0123456789abcdef")
	jetonTest = "123456789:" + strings.Repeat("A", 35)
)

func baseTest(t testing.TB) *sql.DB {
	t.Helper()
	d := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(d, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := base.Migrer(context.Background(), db, filepath.Join(d, "copies")); err != nil {
		t.Fatal(err)
	}
	return db
}

func alerte(id int64, regle, gravite, instance string, debut time.Time, kv ...string) alertes.Alerte {
	etq := labels.FromStrings(append(kv, "alertname", regle, "severity", gravite, "instance", instance)...)
	return alertes.Alerte{ID: id, Regle: regle, Etiquettes: etq, Gravite: gravite, Etat: alertes.Active,
		Debut: debut, ActiveLe: debut, Resume: regle + " sur " + instance, Valeur: 42}
}

type faux struct {
	mu   sync.Mutex
	msgs []*Message
}

func (f *faux) Envoyer(_ context.Context, m *Message, _ *Reglages) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
}

func (f *faux) vider() []*Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.msgs
	f.msgs = nil
	return m
}

// banc : aiguillage en mode actif, horloge et alertes du moteur simulées.
type banc struct {
	t       *testing.T
	db      *sql.DB
	g       *Aiguilleur
	env     *faux
	horloge time.Time
	actives []alertes.Alerte
}

func nouveauBanc(t *testing.T) *banc {
	b := &banc{t: t, db: baseTest(t), env: &faux{}, horloge: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	r := ReglagesDefaut()
	r.Mode = Actif
	r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234"}
	if err := EcrireReglages(context.Background(), b.db, r, cleTest, "test", b.horloge); err != nil {
		t.Fatal(err)
	}
	if _, err := PoserDefauts(context.Background(), b.db, b.horloge); err != nil {
		t.Fatal(err)
	}
	var err error
	b.g, err = Nouveau(context.Background(), Options{DB: b.db, Envoyeur: b.env,
		EnCours:    func() []alertes.Alerte { return b.actives },
		Maintenant: func() time.Time { return b.horloge }})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *banc) debut(a alertes.Alerte) {
	b.actives = append(b.actives, a)
	b.g.Recevoir([]alertes.Evenement{{Type: "debut", Alerte: a}})
}

func (b *banc) fin(a alertes.Alerte) {
	for i := range b.actives {
		if b.actives[i].ID == a.ID {
			b.actives = append(b.actives[:i], b.actives[i+1:]...)
			break
		}
	}
	a.Etat, a.Fin = alertes.Resolue, b.horloge
	b.g.Recevoir([]alertes.Evenement{{Type: "fin", Alerte: a}})
}

func (b *banc) avancer(d time.Duration) []*Message {
	b.horloge = b.horloge.Add(d)
	b.g.Tour(context.Background())
	return b.env.vider()
}

func TestRegroupementEtRappels(t *testing.T) {
	b := nouveauBanc(t)
	t0 := b.horloge
	// Deux disques pleins sur la même machine : un seul message (même groupe).
	d1 := alerte(1, "DisqueFaible", alertes.Avertissement, "pc1", t0, "mountpoint", "/")
	d2 := alerte(2, "DisqueFaible", alertes.Avertissement, "pc1", t0, "mountpoint", "/var")
	b.debut(d1)
	b.debut(d2)
	if m := b.avancer(10 * time.Second); len(m) != 0 {
		t.Fatalf("avant la fin de l'attente (30 s) : %d message(s)", len(m))
	}
	m := b.avancer(25 * time.Second)
	if len(m) != 1 || m[0].Nb() != 2 || m[0].Nouvelles != 2 || m[0].Type != TypeAlerte || m[0].Son {
		t.Fatalf("regroupement : %+v", m)
	}
	// Une critique sur une autre machine : son propre groupe, avec son, 10 s.
	c := alerte(3, "AgentMuet", alertes.Critique, "pc2", b.horloge)
	b.debut(c)
	m = b.avancer(10 * time.Second)
	if len(m) != 1 || m[0].Gravite != alertes.Critique || !m[0].Son {
		t.Fatalf("critique : %+v", m)
	}
	// Fin de d1 : attend l'intervalle de groupe (5 min après le dernier envoi).
	b.fin(d1)
	if m := b.avancer(time.Minute); len(m) != 0 {
		t.Fatalf("fin trop tôt : %+v", m)
	}
	m = b.avancer(4 * time.Minute)
	if len(m) != 1 || m[0].Type != TypeResolution || len(m[0].Resolues) != 1 || len(m[0].Actives) != 1 {
		t.Fatalf("résolution : %+v", m)
	}
	// Rappel de la critique après 2 h ; acquittée : plus de rappel.
	m = b.avancer(2 * time.Hour)
	var rappels int
	for _, x := range m {
		if x.Type == TypeRappel && x.Gravite == alertes.Critique {
			rappels++
		}
	}
	if rappels != 1 {
		t.Fatalf("rappel : %+v", m)
	}
	if _, err := b.db.Exec(`INSERT INTO alertes(id, regle, etiquettes, gravite, etat, debut) VALUES (3, 'AgentMuet', '{}', 'critical', 'active', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := Acquitter(context.Background(), b.db, 3, "moi", "je regarde", time.Time{}, b.horloge); err != nil {
		t.Fatal(err)
	}
	if err := Acquitter(context.Background(), b.db, 99, "moi", "", time.Time{}, b.horloge); err == nil {
		t.Error("acquittement d'une alerte inconnue accepté")
	}
	for _, x := range b.avancer(2*time.Hour + time.Minute) {
		if x.Gravite == alertes.Critique {
			t.Fatalf("rappel après acquittement : %+v", x)
		}
	}
}

// Prise en charge pour une durée (SPEC § 30) : pas de rappel pendant la
// durée ; à la fin, levée et rappel tout de suite ; « Reprendre » la lève.
func TestAcquittementDuree(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	a := alerte(5, "CpuEleve", alertes.Critique, "pc1", b.horloge)
	b.debut(a)
	if m := b.avancer(time.Minute); len(m) != 1 {
		t.Fatalf("premier envoi : %+v", m)
	}
	if _, err := b.db.Exec(`INSERT INTO alertes(id, regle, etiquettes, gravite, etat, debut) VALUES (5, 'CpuEleve', '{}', 'critical', 'active', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := Acquitter(ctx, b.db, 5, "moi", "", b.horloge.Add(-time.Minute), b.horloge); err == nil {
		t.Error("fin de prise en charge passée acceptée")
	}
	if err := Acquitter(ctx, b.db, 5, "moi", "je regarde", b.horloge.Add(30*time.Minute), b.horloge); err != nil {
		t.Fatal(err)
	}
	if m := b.avancer(20 * time.Minute); len(m) != 0 {
		t.Fatalf("rappel pendant la prise en charge : %+v", m)
	}
	m := b.avancer(11 * time.Minute)
	if len(m) != 1 || m[0].Type != TypeRappel || len(m[0].Actives) != 1 || m[0].Actives[0].ID != 5 {
		t.Fatalf("pas de rappel à la fin de la prise en charge : %+v", m)
	}
	var ack int64
	_ = b.db.QueryRow(`SELECT acquittee_le FROM alertes WHERE id = 5`).Scan(&ack)
	if ack != 0 {
		t.Error("prise en charge pas levée à son échéance")
	}
	// Reprendre : lève une prise en charge « jusqu'à la fin ».
	_ = Acquitter(ctx, b.db, 5, "moi", "", time.Time{}, b.horloge)
	if err := Reprendre(ctx, b.db, 5, "moi", b.horloge); err != nil {
		t.Fatal(err)
	}
	if err := Reprendre(ctx, b.db, 5, "moi", b.horloge); err == nil {
		t.Error("reprise d'une alerte pas prise en charge acceptée")
	}
}

// Escalade (SPEC § 30) : alerte toujours pas prise en charge après le délai
// de sa gravité : un message marqué « escalade », une seule fois ; prise en
// charge à temps : pas d'escalade.
func TestEscalade(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	r, _ := LireReglages(ctx, b.db)
	ro := r.Routes[alertes.Critique]
	ro.Escalade = alertes.Duree(15 * time.Minute)
	r.Routes[alertes.Critique] = ro
	if err := EcrireReglages(ctx, b.db, r, cleTest, "test", b.horloge); err != nil {
		t.Fatal(err)
	}
	a := alerte(7, "CpuEleve", alertes.Critique, "pc1", b.horloge)
	a.ActiveLe = b.horloge
	b.debut(a)
	if m := b.avancer(time.Minute); len(m) != 1 || m[0].Escalade {
		t.Fatalf("premier envoi : %+v", m)
	}
	m := b.avancer(15 * time.Minute)
	if len(m) != 1 || !m[0].Escalade || m[0].Actives[0].ID != 7 || !strings.Contains(Texte(m[0], Mise{}), "Escalade") {
		t.Fatalf("pas d'escalade : %+v", m)
	}
	for _, x := range b.avancer(30 * time.Minute) {
		if x.Escalade {
			t.Fatal("escalade envoyée deux fois")
		}
	}
	// Prise en charge à temps : pas d'escalade.
	a2 := alerte(8, "DisqueFaible", alertes.Critique, "pc2", b.horloge)
	a2.ActiveLe = b.horloge
	b.debut(a2)
	b.avancer(time.Minute)
	if _, err := b.db.Exec(`INSERT INTO alertes(id, regle, etiquettes, gravite, etat, debut) VALUES (8, 'DisqueFaible', '{}', 'critical', 'active', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := Acquitter(ctx, b.db, 8, "moi", "", time.Time{}, b.horloge); err != nil {
		t.Fatal(err)
	}
	for _, x := range b.avancer(20 * time.Minute) {
		if x.Escalade {
			t.Fatalf("escalade d'une alerte prise en charge : %+v", x)
		}
	}
}

func TestSourdine(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	t0 := b.horloge
	// Silence d'1 h sur pc1.
	if _, err := AjouterSilence(ctx, b.db, `{instance="pc1"}`, t0, time.Hour, "moi", "travaux", t0); err != nil {
		t.Fatal(err)
	}
	a := alerte(1, "CpuEleve", alertes.Avertissement, "pc1", t0)
	b.debut(a)
	if m := b.avancer(time.Minute); len(m) != 0 {
		t.Fatalf("pendant le silence : %+v", m)
	}
	if r := b.g.RaisonSourdine(&a); !strings.HasPrefix(r, "silence") {
		t.Errorf("raison : %q", r)
	}
	// Silence fini, alerte toujours là : elle part.
	if m := b.avancer(time.Hour); len(m) != 1 || m[0].Actives[0].Regle != "CpuEleve" {
		t.Fatalf("après le silence : %+v", m)
	}

	// Inhibition par défaut : machine muette → ses avertissements sont tus.
	muet := alerte(2, "AgentMuet", alertes.Critique, "pc3", b.horloge)
	disque := alerte(3, "DisqueFaible", alertes.Avertissement, "pc3", b.horloge)
	b.debut(muet)
	b.debut(disque)
	m := b.avancer(time.Minute)
	if len(m) != 1 || m[0].Actives[0].Regle != "AgentMuet" {
		t.Fatalf("inhibition : %+v", m)
	}
	if r := b.g.RaisonSourdine(&disque); !strings.Contains(r, "inhibée par AgentMuet") {
		t.Errorf("raison : %q", r)
	}

	// Dépendance par défaut : collecteur muet → équipements via=collecteur tus,
	// et le message du collecteur le dit.
	nas := alerte(4, "SynologyInjoignable", alertes.Critique, "nas1", b.horloge, "via", "collecteur")
	b.debut(nas)
	b.debut(alerte(5, "AgentMuet", alertes.Critique, "miradorcollector", b.horloge))
	m = b.avancer(time.Minute)
	if len(m) != 1 || m[0].Actives[0].Machine() != "miradorcollector" || m[0].Dependantes != 1 {
		t.Fatalf("dépendance : %+v", m)
	}
	if !strings.Contains(Texte(m[0], Mise{}), "+ 1 alerte(s) d'équipements dépendants") {
		t.Errorf("texte : %s", Texte(m[0], Mise{}))
	}

	// Maintenance ponctuelle sur un groupe de machines.
	if _, err := b.db.Exec(`INSERT INTO machines(nom, groupe, cree_le) VALUES ('pve1', 'proxmox', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := AjouterMaintenance(ctx, b.db, "groupe:proxmox", b.horloge, b.horloge.Add(time.Hour), nil, "moi", "", b.horloge); err != nil {
		t.Fatal(err)
	}
	b.debut(alerte(6, "CpuEleve", alertes.Avertissement, "pve1", b.horloge))
	if m := b.avancer(time.Minute); len(m) != 0 {
		t.Fatalf("pendant la maintenance : %+v", m)
	}
}

func TestRecurrence(t *testing.T) {
	r := Recurrence{Jours: []string{"dim"}, Heure: "23:30", Duree: alertes.Duree(2 * time.Hour)}
	if err := r.Valider(); err != nil {
		t.Fatal(err)
	}
	paris, _ := time.LoadLocation("Europe/Paris")
	cas := map[string]bool{
		"2026-11-15 23:29": false, // dimanche
		"2026-11-15 23:30": true,
		"2026-11-16 01:29": true, // lundi, fenêtre commencée dimanche
		"2026-11-16 01:30": false,
		"2026-11-16 23:45": false,
	}
	for txt, attendu := range cas {
		t0, _ := time.ParseInLocation("2006-01-02 15:04", txt, paris)
		if got := r.enCours(t0); got != attendu {
			t.Errorf("%s : %v, attendu %v", txt, got, attendu)
		}
	}
	for _, mauvaise := range []Recurrence{
		{Jours: []string{"lundi"}, Heure: "03:00", Duree: alertes.Duree(time.Hour)},
		{Jours: []string{"lun"}, Heure: "25:00", Duree: alertes.Duree(time.Hour)},
		{Jours: []string{"lun"}, Heure: "03:00", Duree: alertes.Duree(8 * 24 * time.Hour)},
		{Jours: []string{"lun"}, Heure: "03:00", Duree: alertes.Duree(time.Hour), Fuseau: "Mars/Olympus"},
	} {
		if err := mauvaise.Valider(); err == nil {
			t.Errorf("récurrence acceptée : %+v", mauvaise)
		}
	}
}

// Rythmes jour et mois, prochaine fenêtre, texte (SPEC § 81).
func TestRecurrenceRythmes(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	a := func(txt string) time.Time {
		t0, _ := time.ParseInLocation("2006-01-02 15:04", txt, paris)
		return t0
	}
	jour := Recurrence{Rythme: "jour", Jours: []string{"lun"}, Heure: "02:00", Duree: alertes.Duree(time.Hour)}
	mois := Recurrence{Rythme: "mois", JourMois: 31, Heure: "03:00", Duree: alertes.Duree(2 * time.Hour)}
	for _, r := range []*Recurrence{&jour, &mois} {
		if err := r.Valider(); err != nil {
			t.Fatal(err)
		}
	}
	if jour.Jours != nil || !jour.enCours(a("2026-11-19 02:30")) || jour.enCours(a("2026-11-19 03:00")) {
		t.Errorf("chaque jour : %+v", jour)
	}
	// 31 = dernier jour : 30/11 en novembre, 28/02 en 2027.
	for txt, attendu := range map[string]bool{"2026-11-30 04:00": true, "2026-11-29 04:00": false,
		"2026-12-31 03:10": true, "2027-02-28 03:00": true, "2027-03-01 01:00": false} {
		if got := mois.enCours(a(txt)); got != attendu {
			t.Errorf("mois %s : %v, attendu %v", txt, got, attendu)
		}
	}
	if d, f := mois.Prochaine(a("2026-11-20 10:00")); !d.Equal(a("2026-11-30 03:00")) || !f.Equal(a("2026-11-30 05:00")) {
		t.Errorf("prochaine mensuelle : %v → %v", d, f)
	}
	sem := Recurrence{Jours: []string{"dim"}, Heure: "03:00", Duree: alertes.Duree(time.Hour)}
	_ = sem.Valider()
	// 22/11/2026 est un dimanche ; une fenêtre en cours est rendue telle quelle.
	for depuis, attendu := range map[string]string{"2026-11-20 10:00": "2026-11-22 03:00", "2026-11-22 03:30": "2026-11-22 03:00",
		"2026-11-22 04:00": "2026-11-29 03:00"} {
		if d, _ := sem.Prochaine(a(depuis)); !d.Equal(a(attendu)) {
			t.Errorf("prochaine hebdo depuis %s : %v", depuis, d)
		}
	}
	for r, attendu := range map[*Recurrence]string{&jour: "chaque jour à 02:00 pendant 1h", &mois: "le dernier jour de chaque mois à 03:00 pendant 2h",
		&sem: "chaque dim à 03:00 pendant 1h"} {
		if got := r.Texte(); got != attendu {
			t.Errorf("texte %q, attendu %q", got, attendu)
		}
	}
	for _, mauvaise := range []Recurrence{
		{Rythme: "mois", JourMois: 0, Heure: "03:00", Duree: alertes.Duree(time.Hour)},
		{Rythme: "an", Heure: "03:00", Duree: alertes.Duree(time.Hour)},
	} {
		if err := mauvaise.Valider(); err == nil {
			t.Errorf("acceptée : %+v", mauvaise)
		}
	}
}

func TestRedemarrageEtFinJamaisAnnoncee(t *testing.T) {
	b := nouveauBanc(t)
	t0 := b.horloge
	a := alerte(1, "CpuEleve", alertes.Avertissement, "pc1", t0)
	b.actives = []alertes.Alerte{a}
	// Redémarrage : l'alerte en cours n'est pas renvoyée.
	g, err := Nouveau(context.Background(), Options{DB: b.db, Envoyeur: b.env,
		EnCours: func() []alertes.Alerte { return b.actives }, Maintenant: func() time.Time { return b.horloge }})
	if err != nil {
		t.Fatal(err)
	}
	b.g = g
	if m := b.avancer(time.Minute); len(m) != 0 {
		t.Fatalf("après redémarrage : %+v", m)
	}
	// Début puis fin avant la fin de l'attente : rien du tout.
	x := alerte(2, "Bref", alertes.Info, "pc9", b.horloge)
	b.debut(x)
	b.avancer(5 * time.Second)
	b.fin(x)
	if m := b.avancer(time.Minute); len(m) != 0 {
		t.Fatalf("alerte jamais annoncée : %+v", m)
	}
}

func TestModeOmbre(t *testing.T) {
	b := nouveauBanc(t)
	r := ReglagesDefaut() // mode ombre par défaut
	if err := EcrireReglages(context.Background(), b.db, r, cleTest, "test", b.horloge); err != nil {
		t.Fatal(err)
	}
	b.debut(alerte(1, "CpuEleve", alertes.Critique, "pc1", b.horloge))
	if m := b.avancer(time.Minute); len(m) != 0 {
		t.Fatalf("mode ombre : envoyé %+v", m)
	}
	es, err := ListerEnvois(context.Background(), b.db, 10)
	if err != nil || len(es) != 1 || es[0].Statut != Ombre || !strings.Contains(es[0].Texte, "CpuEleve") {
		t.Fatalf("historique : %+v %v", es, err)
	}
}

func TestTexte(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	a := alerte(7, "CpuEleve", alertes.Critique, "pc<1>", t0)
	a.Resume = `<script>alert(1)</script> & co`
	m := &Message{Le: t0.Add(90 * time.Minute), Type: TypeAlerte, Actives: []alertes.Alerte{a}, Nouvelles: 1}
	paris, _ := time.LoadLocation("Europe/Paris")
	h := Texte(m, Mise{HTML: true, Fuseau: paris, Lien: "https://mirador.example.org"})
	for _, attendu := range []string{"🔴 <b>CpuEleve</b>", "&lt;script&gt;alert(1)&lt;/script&gt; &amp; co",
		"<code>pc&lt;1&gt;</code> · depuis 10/11 13:00 (1h 30m) · valeur 42", "https://mirador.example.org/alertes/7"} {
		if !strings.Contains(h, attendu) {
			t.Errorf("manque %q dans :\n%s", attendu, h)
		}
	}
	if strings.Contains(h, "<script>") {
		t.Error("HTML non échappé")
	}
	// Coupé proprement.
	var beaucoup []alertes.Alerte
	for i := 0; i < 200; i++ {
		beaucoup = append(beaucoup, a)
	}
	long := Texte(&Message{Le: t0, Actives: beaucoup}, Mise{HTML: true, Max: 4096})
	if len(long) > 4096 || !strings.HasSuffix(long, "voir l'interface)") || strings.Count(long, "<b>") != strings.Count(long, "</b>") {
		t.Errorf("coupe : %d octets", len(long))
	}
}

func TestValidationReglages(t *testing.T) {
	ok := ReglagesDefaut()
	if err := ok.Valider(); err != nil {
		t.Fatal(err)
	}
	for i, f := range []func(r *Reglages){
		func(r *Reglages) { r.Mode = "fort" },
		func(r *Reglages) { r.Lien = "http://mirador.example.org" },
		func(r *Reglages) { r.Fuseau = "Nulle/Part" },
		func(r *Reglages) { r.GrouperPar = []string{"a b"} },
		func(r *Reglages) {
			ro := r.Routes[alertes.Critique]
			ro.Rappel = alertes.Duree(time.Minute)
			r.Routes[alertes.Critique] = ro
		},
	} {
		r := ReglagesDefaut()
		f(&r)
		if err := r.Valider(); err == nil {
			t.Errorf("cas %d accepté", i)
		}
	}
	db := baseTest(t)
	for _, sel := range []string{"instance=pc1", `{instance=~"(("}`, strings.Repeat("{", 2000), "{}", `{instance=~".*"}`} {
		if _, err := AjouterSilence(context.Background(), db, sel, time.Now(), time.Hour, "t", "", time.Now()); err == nil {
			t.Errorf("sélecteur %q accepté", sel[:min(len(sel), 20)])
		}
	}
	if _, err := AjouterDependance(context.Background(), db, "machine:a", "a", "t", "", time.Now()); err == nil {
		t.Error("machine dépendante d'elle-même acceptée")
	}
}

// Maintenance par site (SPEC § 81) : machines du site, équipements de son
// collecteur, alertes portant l'étiquette site ; les autres sites partent.
func TestMaintenanceSite(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO machines(id, nom, site, type, cree_le) VALUES (1, 'col-bureau', 'bureau', 'collecteur', 0)`,
		`INSERT INTO machines(id, nom, site, cree_le) VALUES (2, 'pc-bureau', 'bureau', 0)`,
		`INSERT INTO machines(id, nom, cree_le) VALUES (3, 'nas-bureau', 0)`,
		`INSERT INTO equipements_collecteur(machine_id, collecteur_id, rattache_le) VALUES (3, 1, 0)`,
		`INSERT INTO machines(id, nom, site, cree_le) VALUES (4, 'pc-maison', 'maison', 0)`,
	} {
		if _, err := b.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AjouterMaintenance(ctx, b.db, "site:bureau", b.horloge, b.horloge.Add(time.Hour), nil, "moi", "", b.horloge); err != nil {
		t.Fatal(err)
	}
	s, err := Charger(ctx, b.db, nil, b.horloge)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		a   alertes.Alerte
		tue bool
	}{
		{alerte(1, "CpuEleve", alertes.Avertissement, "pc-bureau", b.horloge), true},
		{alerte(2, "SynologyInjoignable", alertes.Critique, "nas-bureau", b.horloge), true},
		{alerte(3, "SondeEnEchec", alertes.Critique, "lan", b.horloge, "site", "bureau"), true},
		{alerte(4, "CpuEleve", alertes.Avertissement, "pc-maison", b.horloge), false},
	} {
		if got := s.Raison(&c.a, nil, b.horloge) != ""; got != c.tue {
			t.Errorf("%s : en sourdine %v, attendu %v", c.a.Machine(), got, c.tue)
		}
	}
}
