// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"bytes"
	"context"
	"database/sql"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/web"
)

// Tokens de test (fictifs, propres à ces tests).
const (
	tokenPC         = "token-de-test-pc-0123456789"
	tokenAttente    = "token-de-test-attente-0123"
	tokenRevoque    = "token-de-test-revoque-0123"
	tokenCollecteur = "token-de-test-collecteur-01"
)

type banc struct {
	g     *Ingestion
	mem   *stockage.Memoire
	db    *sql.DB
	h     http.Handler
	heure time.Time
}

func nouveauBanc(t *testing.T, modifier func(*Options)) *banc {
	t.Helper()
	ctx := context.Background()
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		id               int
		nom, statut, typ string
		token            string
		revoque          int64
	}{
		{1, "pc", "approuvee", "agent", tokenPC, 0},
		{2, "nouveau", "en_attente", "agent", tokenAttente, 0},
		{3, "ancien", "approuvee", "agent", tokenRevoque, 1},
		{4, "collecteur-lan", "approuvee", "collecteur", tokenCollecteur, 0},
	} {
		if _, err := db.Exec(`INSERT INTO machines(id, nom, statut, type, cree_le) VALUES (?, ?, ?, ?, 1)`,
			m.id, m.nom, m.statut, m.typ); err != nil {
			t.Fatal(err)
		}
		e := Empreinte(m.token)
		if _, err := db.Exec(`INSERT INTO tokens(machine_id, empreinte, cree_le, revoque_le) VALUES (?, ?, 1, ?)`,
			m.id, e[:], m.revoque); err != nil {
			t.Fatal(err)
		}
	}
	b := &banc{mem: &stockage.Memoire{}, db: db, heure: reception}
	opt := Options{DB: db, Metriques: b.mem, Logs: stockage.LogsMemoire{Memoire: b.mem},
		Maintenant: func() time.Time { return b.heure }}
	if modifier != nil {
		modifier(&opt)
	}
	b.g = Nouvelle(opt)
	m := http.NewServeMux()
	b.g.Routes(m)
	b.h = web.IPClient(nil)(m)
	return b
}

// envoyer fait une requête ; ip vide = 192.0.2.1.
func (b *banc) envoyer(chemin, token, encodage string, corps []byte, entetes ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, chemin, bytes.NewReader(corps))
	r.RemoteAddr = "192.0.2.1:4000"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if encodage != "" {
		r.Header.Set("Content-Encoding", encodage)
	}
	for i := 0; i+1 < len(entetes); i += 2 {
		if entetes[i] == "ip" {
			r.RemoteAddr = net.JoinHostPort(entetes[i+1], "4000")
			continue
		}
		r.Header.Set(entetes[i], entetes[i+1])
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	return w
}

func (b *banc) metriques(token string, corps []byte, entetes ...string) int {
	return b.envoyer(protocole.CheminMetriques, token, "snappy", corps, entetes...).Code
}

func requeteSimple(t int64, labels ...[2]string) []byte {
	return pbRequete(pbSerie(append([][2]string{{"__name__", "node_load1"}}, labels...), pbPoint(0.5, t)))
}

func TestAuthentification(t *testing.T) {
	b := nouveauBanc(t, nil)
	corps := requeteSimple(reception.UnixMilli())
	cas := map[string]int{
		"":                        http.StatusUnauthorized,
		"court":                   http.StatusUnauthorized,
		"token-inconnu-012345678": http.StatusUnauthorized,
		tokenAttente:              http.StatusUnauthorized, // machine pas encore approuvée
		tokenRevoque:              http.StatusUnauthorized,
		tokenPC:                   http.StatusNoContent,
	}
	for tok, attendu := range cas {
		if code := b.metriques(tok, corps, "ip", "198.51.100.7"); code != attendu {
			t.Errorf("token %q : %d, attendu %d", tok, code, attendu)
		}
	}
	// Schéma « bearer » insensible à la casse ; autre schéma refusé.
	r := b.envoyer(protocole.CheminMetriques, "", "snappy", corps, "Authorization", "bearer "+tokenPC, "ip", "198.51.100.8")
	if r.Code != http.StatusNoContent {
		t.Errorf("bearer minuscule : %d", r.Code)
	}
	r = b.envoyer(protocole.CheminMetriques, "", "snappy", corps, "Authorization", "Basic "+tokenPC, "ip", "198.51.100.8")
	if r.Code != http.StatusUnauthorized {
		t.Errorf("Basic : %d", r.Code)
	}
}

func TestRevocationEtOubli(t *testing.T) {
	b := nouveauBanc(t, nil)
	corps := requeteSimple(reception.UnixMilli())
	if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	e := Empreinte(tokenPC)
	if _, err := b.db.Exec(`UPDATE tokens SET revoque_le = 5 WHERE empreinte = ?`, e[:]); err != nil {
		t.Fatal(err)
	}
	// Encore en cache…
	if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
		t.Fatalf("cache : %d", c)
	}
	// … refusé dès Oublier, ou au plus tard après la durée du cache.
	b.g.Auth().Oublier()
	if c := b.metriques(tokenPC, corps); c != http.StatusUnauthorized {
		t.Fatalf("après révocation : %d", c)
	}
}

func TestEchecsRepetesBloques(t *testing.T) {
	b := nouveauBanc(t, nil)
	corps := requeteSimple(reception.UnixMilli())
	for i := 0; i < 10; i++ {
		if c := b.metriques("token-faux-0123456789-"+strconv.Itoa(i), corps, "ip", "203.0.113.9"); c != http.StatusUnauthorized {
			t.Fatalf("essai %d : %d", i, c)
		}
	}
	// Même un bon token est refusé depuis cette IP pendant un moment…
	if c := b.metriques(tokenPC, corps, "ip", "203.0.113.9"); c != http.StatusTooManyRequests {
		t.Fatalf("après 10 échecs : %d", c)
	}
	// … mais pas depuis une autre IP.
	if c := b.metriques(tokenPC, corps, "ip", "203.0.113.10"); c != http.StatusNoContent {
		t.Fatalf("autre IP : %d", c)
	}
	b.heure = b.heure.Add(10 * time.Second) // 0,2 échec/s : 2 jetons reviennent
	if c := b.metriques(tokenPC, corps, "ip", "203.0.113.9"); c != http.StatusNoContent {
		t.Fatalf("après attente : %d", c)
	}
}

func TestNomImpose(t *testing.T) {
	b := nouveauBanc(t, nil)
	// Un agent qui se fait passer pour une autre machine : instance remplacée.
	corps := requeteSimple(reception.UnixMilli(), [2]string{"instance", "serveur-de-paie"}, [2]string{"job", "integrations/unix"})
	if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	s := b.mem.Series[0]
	if s.Valeur("instance") != "pc" || s.Valeur("job") != "integrations/unix" {
		t.Fatalf("étiquettes : %+v", s.Labels)
	}
	// Sans instance : ajoutée, étiquettes toujours triées.
	if c := b.metriques(tokenPC, requeteSimple(reception.UnixMilli(), [2]string{"zone", "a"})); c != http.StatusNoContent {
		t.Fatal(c)
	}
	s = b.mem.Series[1]
	if s.Valeur("instance") != "pc" || s.Labels[0].Nom != "__name__" || s.Labels[1].Nom != "instance" || s.Labels[2].Nom != "zone" {
		t.Fatalf("étiquettes : %+v", s.Labels)
	}
	// Collecteur : garde l'instance de l'équipement, son nom va dans « collecteur ».
	if c := b.metriques(tokenCollecteur, requeteSimple(reception.UnixMilli(), [2]string{"instance", "nas"})); c != http.StatusNoContent {
		t.Fatal(c)
	}
	s = b.mem.Series[2]
	if s.Valeur("instance") != "nas" || s.Valeur("collecteur") != "collecteur-lan" {
		t.Fatalf("collecteur : %+v", s.Labels)
	}
}

func TestHorloge(t *testing.T) {
	b := nouveauBanc(t, nil)
	heure := func(d time.Duration) string { return strconv.FormatInt(reception.Add(d).UnixMilli(), 10) }

	// Agent en avance de 3 min : horodatages ramenés à l'heure du serveur.
	// Le lot a été produit 1 min avant l'envoi (file d'attente) : l'écart est conservé.
	corps := requeteSimple(reception.Add(3*time.Minute - time.Minute).UnixMilli())
	if c := b.metriques(tokenPC, corps, protocole.EnteteHeure, heure(3*time.Minute)); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if got, want := b.mem.Series[0].Points[0].T, reception.Add(-time.Minute).UnixMilli(); got != want {
		t.Errorf("point corrigé à %d, attendu %d", got, want)
	}
	if d, ok := b.g.Decalage(1); !ok || d != 3*time.Minute {
		t.Errorf("décalage noté : %v", d)
	}
	// Retard de 20 s : toléré, rien n'est changé.
	corps = requeteSimple(reception.Add(-20 * time.Second).UnixMilli())
	if c := b.metriques(tokenPC, corps, protocole.EnteteHeure, heure(-20*time.Second)); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if got := b.mem.Series[1].Points[0].T; got != reception.Add(-20*time.Second).UnixMilli() {
		t.Errorf("point toléré modifié : %d", got)
	}
	// Retard d'une heure : refusé (400 → l'agent abandonne le lot).
	if c := b.metriques(tokenPC, corps, protocole.EnteteHeure, heure(-time.Hour)); c != http.StatusBadRequest {
		t.Errorf("décalage d'une heure : %d", c)
	}
	if c := b.metriques(tokenPC, corps, protocole.EnteteHeure, "demain"); c != http.StatusBadRequest {
		t.Errorf("en-tête illisible : %d", c)
	}
	// Sans en-tête : seuls les points trop loin dans le futur sont écartés.
	futur := pbRequete(pbSerie([][2]string{{"__name__", "a"}}, pbPoint(1, reception.Add(time.Hour).UnixMilli()), pbPoint(2, reception.UnixMilli())))
	if c := b.metriques(tokenPC, futur); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if p := b.mem.Series[2].Points; len(p) != 1 || p[0].V != 2 || b.g.Compteurs.PointsFuturs.Load() != 1 {
		t.Errorf("points futurs : %+v", p)
	}
	if b.g.Compteurs.HorlogeCorrigee.Load() != 1 || b.g.Compteurs.HorlogeRefusee.Load() != 1 {
		t.Error("compteurs d'horloge")
	}
}

func TestQuotaDeSeries(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) {
		o.Series = LimitesSeries{ParMachine: 100, ParNom: 3, Total: 1000, Inactivite: time.Hour}
	})
	var series [][]byte
	for i := 0; i < 5; i++ {
		series = append(series, pbSerie([][2]string{{"__name__", "requetes"}, {"id_session", strconv.Itoa(i)}}, pbPoint(1, reception.UnixMilli())))
	}
	series = append(series, pbSerie([][2]string{{"__name__", "node_load1"}}, pbPoint(1, reception.UnixMilli())))
	// Les séries en trop sont refusées, le reste du lot est gardé (204).
	if c := b.metriques(tokenPC, pbRequete(series...)); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if len(b.mem.Series) != 4 || b.g.Compteurs.SeriesRefusees.Load() != 2 {
		t.Fatalf("gardées %d, refusées %d", len(b.mem.Series), b.g.Compteurs.SeriesRefusees.Load())
	}
}

func TestDebitParMachine(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.RequetesMachine = 1 }) // rafale 10
	corps := requeteSimple(reception.UnixMilli())
	for i := 0; i < 10; i++ {
		if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
			t.Fatalf("requête %d : %d", i, c)
		}
	}
	w := b.envoyer(protocole.CheminMetriques, tokenPC, "snappy", corps)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("11e requête : %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestRequetesInvalides(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.MaxCorpsMetriques = 1000 })
	if w := b.envoyer(protocole.CheminMetriques, tokenPC, "gzip", requeteSimple(1)); w.Code != http.StatusBadRequest {
		t.Errorf("mauvais encodage : %d", w.Code)
	}
	if c := b.metriques(tokenPC, []byte("n'importe quoi")); c != http.StatusBadRequest {
		t.Errorf("corps invalide : %d", c)
	}
	if c := b.metriques(tokenPC, make([]byte, 2000)); c != http.StatusRequestEntityTooLarge {
		t.Errorf("corps trop gros : %d", c)
	}
	if w := b.envoyer(protocole.CheminLogs, tokenPC, "br", []byte("x")); w.Code != http.StatusBadRequest {
		t.Errorf("logs, mauvais encodage : %d", w.Code)
	}
}

func TestServeurOccupe(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.MaxSimultanees = 1 })
	b.g.places <- struct{}{} // une requête en cours
	if c := b.metriques(tokenPC, requeteSimple(reception.UnixMilli())); c != http.StatusServiceUnavailable {
		t.Fatalf("serveur occupé : %d", c)
	}
	<-b.g.places
	if c := b.metriques(tokenPC, requeteSimple(reception.UnixMilli())); c != http.StatusNoContent {
		t.Fatalf("place libérée : %d", c)
	}
}

func lotLogs(n int, host string, t0 time.Time) []byte {
	lot := logs.NewLot(host, "journal", "maison", "prod", 1<<20)
	for i := 0; i < n; i++ {
		lot.Ajouter(t0, "ligne "+strconv.Itoa(i), logs.Champs{{"unit", "ssh.service"}})
	}
	corps, _ := lot.Vider()
	return corps
}

func TestLogs(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.LignesMinute = 100 })
	// L'agent annonce un autre nom : host remplacé par celui du token.
	corps := lotLogs(60, "serveur-de-paie", reception.Add(2*time.Minute))
	w := b.envoyer(protocole.CheminLogs, tokenPC, "gzip", corps,
		protocole.EnteteHeure, strconv.FormatInt(reception.Add(2*time.Minute).UnixMilli(), 10))
	if w.Code != http.StatusNoContent {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(b.mem.Lignes) != 60 {
		t.Fatal(len(b.mem.Lignes))
	}
	l := b.mem.Lignes[0]
	if l.Champs["host"] != "pc" || l.Champs["unit"] != "ssh.service" || l.T != reception.UnixMilli() {
		t.Errorf("ligne : %+v", l)
	}
	// Quota de 100 lignes/min : 60 + 60 passe (rafale), le lot suivant attend.
	if w := b.envoyer(protocole.CheminLogs, tokenPC, "gzip", lotLogs(60, "pc", reception)); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	w = b.envoyer(protocole.CheminLogs, tokenPC, "gzip", lotLogs(60, "pc", reception))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("quota de logs : %d", w.Code)
	}
	if b.g.Compteurs.LignesRefusees.Load() != 60 {
		t.Error("lignes refusées non comptées")
	}
	// Logs non compressés acceptés aussi.
	b.heure = b.heure.Add(2 * time.Minute)
	if w := b.envoyer(protocole.CheminLogs, tokenPC, "", []byte(`{"_msg":"brut"}`)); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
}

func TestVuLe(t *testing.T) {
	b := nouveauBanc(t, nil)
	if c := b.metriques(tokenPC, requeteSimple(reception.UnixMilli())); c != http.StatusNoContent {
		t.Fatal(c)
	}
	var vu int64
	_ = b.db.QueryRow(`SELECT vu_le FROM machines WHERE id = 1`).Scan(&vu)
	if vu != 0 {
		t.Fatal("vu_le ne doit être écrit qu'en lot")
	}
	if err := b.g.Auth().EcrireVus(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = b.db.QueryRow(`SELECT vu_le FROM machines WHERE id = 1`).Scan(&vu)
	if vu != reception.Unix() {
		t.Fatalf("vu_le = %d", vu)
	}
}

func TestBoucleSArrete(t *testing.T) {
	b := nouveauBanc(t, nil)
	ctx, annuler := context.WithCancel(context.Background())
	fin := make(chan struct{})
	go func() { b.g.Boucle(ctx); close(fin) }()
	if c := b.metriques(tokenPC, requeteSimple(reception.UnixMilli())); c != http.StatusNoContent {
		t.Fatal(c)
	}
	annuler()
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("la boucle ne s'arrête pas")
	}
	var vu int64
	_ = b.db.QueryRow(`SELECT vu_le FROM machines WHERE id = 1`).Scan(&vu)
	if vu != reception.Unix() {
		t.Fatal("vu_le doit être écrit à l'arrêt")
	}
}

// BenchmarkIngestionMetriques : coût complet d'une requête d'agent (800 séries).
func BenchmarkIngestionMetriques(b *testing.B) {
	bc := nouveauBancB(b)
	var series [][]byte
	for i := 0; i < 800; i++ {
		series = append(series, pbSerie([][2]string{{"__name__", "node_cpu_seconds_total"}, {"cpu", strconv.Itoa(i)}, {"mode", "user"}}, pbPoint(float64(i), reception.UnixMilli())))
	}
	corps := pbRequete(series...)
	b.ReportAllocs()
	for b.Loop() {
		bc.mem.Series = bc.mem.Series[:0]
		if c := bc.metriques(tokenPC, corps); c != http.StatusNoContent {
			b.Fatal(c)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/800, "ns/série")
}

// nouveauBancB : banc pour les mesures (sans limite de débit gênante).
func nouveauBancB(b *testing.B) *banc {
	b.Helper()
	db, err := base.Ouvrir(filepath.Join(b.TempDir(), "mirador.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if _, err := base.Migrer(context.Background(), db, b.TempDir()); err != nil {
		b.Fatal(err)
	}
	e := Empreinte(tokenPC)
	_, _ = db.Exec(`INSERT INTO machines(id, nom, statut, cree_le) VALUES (1, 'pc', 'approuvee', 1)`)
	_, _ = db.Exec(`INSERT INTO tokens(machine_id, empreinte, cree_le) VALUES (1, ?, 1)`, e[:])
	bc := &banc{mem: &stockage.Memoire{}, db: db, heure: reception}
	bc.g = Nouvelle(Options{DB: db, Metriques: bc.mem, Logs: stockage.LogsMemoire{Memoire: bc.mem},
		RequetesMachine: 1e9, RequetesIP: 1e9, Maintenant: func() time.Time { return bc.heure }})
	m := http.NewServeMux()
	bc.g.Routes(m)
	bc.h = web.IPClient(nil)(m)
	return bc
}

func TestTokenLieAuxIP(t *testing.T) {
	b := nouveauBanc(t, nil)
	corps := requeteSimple(reception.UnixMilli())
	// Sans liste : toute adresse (déjà filtrée par nginx) est admise.
	if c := b.metriques(tokenPC, corps, "ip", "198.51.100.20"); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if _, err := b.db.Exec(`UPDATE machines SET ips_autorisees = '192.0.2.1, 2001:db8:1:2::/64' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	b.g.Auth().Oublier() // ce que fera l'interface après un changement
	cas := map[string]int{
		"192.0.2.1":          http.StatusNoContent,
		"2001:db8:1:2::abcd": http.StatusNoContent, // adresse IPv6 changeante, même /64
		"198.51.100.20":      http.StatusForbidden,
		"2001:db8:1:3::1":    http.StatusForbidden,
		"::ffff:192.0.2.1":   http.StatusNoContent, // IPv4 écrite en IPv6
	}
	for ip, attendu := range cas {
		if c := b.metriques(tokenPC, corps, "ip", ip); c != attendu {
			t.Errorf("%s : %d, attendu %d", ip, c, attendu)
		}
	}
	if b.g.Compteurs.Refus403.Load() != 2 {
		t.Errorf("refus 403 : %d", b.g.Compteurs.Refus403.Load())
	}
	// Liste illisible : tout est refusé (jamais tout accepté).
	if _, err := b.db.Exec(`UPDATE machines SET ips_autorisees = 'n''importe quoi' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	b.g.Auth().Oublier()
	if c := b.metriques(tokenPC, corps, "ip", "192.0.2.1"); c != http.StatusForbidden {
		t.Errorf("liste illisible : %d", c)
	}
}

func TestDerniereIP(t *testing.T) {
	b := nouveauBanc(t, nil)
	if c := b.metriques(tokenPC, requeteSimple(reception.UnixMilli()), "ip", "203.0.113.50"); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if err := b.g.Auth().EcrireVus(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ip string
	_ = b.db.QueryRow(`SELECT derniere_ip FROM machines WHERE id = 1`).Scan(&ip)
	if ip != "203.0.113.50" {
		t.Fatalf("derniere_ip = %q", ip)
	}
}

func TestLireIPs(t *testing.T) {
	p, err := LireIPs(" 203.0.113.7 ,198.51.100.9/24,, 2001:db8::1 ")
	if err != nil || len(p) != 3 || p[1].String() != "198.51.100.0/24" || p[2].Bits() != 128 {
		t.Fatalf("%v %v", p, err)
	}
	for _, faux := range []string{"300.1.1.1", "10.0.0.0/33", "machine.lan"} {
		if _, err := LireIPs(faux); err == nil {
			t.Errorf("%q devrait être refusé", faux)
		}
	}
}

// Site et type (env) de la fiche machine : imposés aux métriques et aux
// logs d'un agent ; vides = ceux de l'agent gardés ; jamais pour un collecteur.
func TestSiteEnvImposes(t *testing.T) {
	b := nouveauBanc(t, nil)
	corps := requeteSimple(reception.UnixMilli(), [2]string{"site", "ancien"}, [2]string{"env", "vm"})
	if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if s := b.mem.Series[0]; s.Valeur("site") != "ancien" || s.Valeur("env") != "vm" {
		t.Fatalf("fiche vide : étiquettes de l'agent attendues : %+v", s.Labels)
	}
	if _, err := b.db.Exec(`UPDATE machines SET site = 'maison', env = 'vps', groupe = 'dns'`); err != nil {
		t.Fatal(err)
	}
	b.g.Auth().Oublier()
	if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if s := b.mem.Series[1]; s.Valeur("site") != "maison" || s.Valeur("env") != "vps" || s.Valeur("groupe") != "dns" {
		t.Fatalf("fiche : %+v", s.Labels)
	}
	if c := b.metriques(tokenCollecteur, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if s := b.mem.Series[2]; s.Valeur("site") != "ancien" {
		t.Fatalf("collecteur modifié : %+v", s.Labels)
	}
	if w := b.envoyer(protocole.CheminLogs, tokenPC, "gzip", lotLogs(1, "pc", reception)); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	if l := b.mem.Lignes[0]; l.Champs["site"] != "maison" || l.Champs["env"] != "vps" || l.Champs["groupe"] != "dns" {
		t.Fatalf("logs : %+v", l.Champs)
	}
}

// Lignes de logs comptées par hôte (courbes de volume).
func TestLignesParHote(t *testing.T) {
	b := nouveauBanc(t, nil)
	if w := b.envoyer(protocole.CheminLogs, tokenPC, "gzip", lotLogs(7, "pc", reception)); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	if n := b.g.LignesParHote()["pc"]; n != 7 {
		t.Fatalf("pc : %d lignes comptées", n)
	}
}
