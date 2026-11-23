// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
)

// metriquesSimulees : base de métriques de test, un point toutes les 30 s
// sur 25 h jusqu'à maintenant ; f(k, ts) donne la valeur (k = n° du point).
type serieTest struct {
	etq []string
	f   func(k int) float64
}

func (b *banc) metriquesSimulees(t *testing.T, series []serieTest) {
	t.Helper()
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := b.h.maintenant()
	app := db.Appender(context.Background())
	k := 0
	for ts := m.Add(-25 * time.Hour); !ts.After(m); ts = ts.Add(30 * time.Second) {
		for _, s := range series {
			if v := s.f(k); v == v { // NaN : pas de point
				_, _ = app.Append(0, labels.FromStrings(s.etq...), ts.UnixMilli(), v)
			}
		}
		k++
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
}

func constante(v float64) func(int) float64 { return func(int) float64 { return v } }

func (b *banc) machinesApprouvees(t *testing.T, noms ...string) {
	t.Helper()
	for _, n := range noms {
		if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, vu_le) VALUES (?, 'approuvee', 'agent', 1, ?)`, n, b.h.maintenant().Unix()); err != nil {
			t.Fatal(err)
		}
	}
}

// Blocs Services et Paquets de la page machine (SPEC § 42.2 et 42.3) :
// après les alertes, avant « En ce moment » ; ouverts d'office s'il y a un
// problème ; services de base cachés ; liens vers les logs.
func TestBlocsServicesEtPaquets(t *testing.T) {
	b := nouveauBanc(t)
	u := func(nom string) []string {
		return []string{"__name__", "node_systemd_unit_state", "instance", "web1", "name", nom, "state", "failed"}
	}
	en := func(nom string) []string {
		return []string{"__name__", "node_systemd_unit_enabled_active", "instance", "web1", "name", nom}
	}
	pq := func(nom, avant, apres, sec string) []string {
		return []string{"__name__", "apt_upgrade_package", "instance", "web1", "package", nom, "from", avant, "to", apres, "security", sec}
	}
	b.metriquesSimulees(t, []serieTest{
		{[]string{"__name__", "node_cpu_seconds_total", "instance", "web1", "cpu", "0", "mode", "idle"}, func(k int) float64 { return float64(k) * 25 }},
		{u("nginx.service"), constante(0)}, {en("nginx.service"), constante(1)},
		{u("fail2ban.service"), constante(1)}, {en("fail2ban.service"), constante(0)},
		{u("cron.service"), constante(0)}, {en("cron.service"), constante(1)},
		{u("docker.service"), constante(0)}, {en("docker.service"), constante(0)},
		{u("sauvegarde.service"), constante(0)},
		{u("systemd-journald.service"), constante(0)},
		{u("ssh.socket"), constante(0)}, // pas un service : ignoré
		{[]string{"__name__", "node_systemd_service_restart_total", "instance", "web1", "name", "fail2ban.service"}, func(k int) float64 { return float64(k / 1000) }},
		{[]string{"__name__", "apt_upgrades_pending", "instance", "web1"}, constante(3)},
		{[]string{"__name__", "apt_upgrades_security_pending", "instance", "web1"}, constante(1)},
		{[]string{"__name__", "node_reboot_required", "instance", "web1"}, constante(1)},
		{pq("openssl", "3.0.15-1", "3.0.17-1", "1"), constante(1)},
		{pq("curl", "7.88.1-10+deb12u8", "7.88.1-10+deb12u12", "0"), constante(1)},
		{pq("tzdata", "2025a", "2025b", "0"), constante(1)},
	})
	b.machinesApprouvees(t, "web1")
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/web1")
	svc := blocDetails(corps, `id="services"`)
	for _, attendu := range []string{`data-ouvrir`, "1 en échec", "1 arrêté", "2 actifs", "1 à la demande",
		`<tr data-groupes="echec demarrage surveille"><td><strong>fail2ban.service</strong>`, `href="/logs?host=web1&amp;unit=nginx.service"`,
		"1 service du système (systemd-*, getty, dbus…) caché.", `data-filtre="echec">À regarder (2)`} {
		if !strings.Contains(svc, attendu) {
			t.Errorf("bloc Services : %q absent", attendu)
		}
	}
	for _, absent := range []string{"systemd-journald", "ssh.socket"} {
		if strings.Contains(svc, absent) {
			t.Errorf("bloc Services : %q ne devrait pas apparaître", absent)
		}
	}
	// fail2ban (échec) avant docker (arrêté) avant nginx (actif) avant sauvegarde (à la demande).
	if !(strings.Index(svc, "fail2ban") < strings.Index(svc, "docker.service") && strings.Index(svc, "docker.service") < strings.Index(svc, "nginx") &&
		strings.Index(svc, "nginx") < strings.Index(svc, "sauvegarde")) {
		t.Error("ordre des services")
	}
	pqs := blocDetails(corps, `id="paquets"`)
	for _, attendu := range []string{"3, dont 1 de sécurité", "Redémarrage nécessaire", "<strong>openssl</strong>", "deb12u12", ">Sécurité<"} {
		if !strings.Contains(pqs, attendu) {
			t.Errorf("bloc Paquets : %q absent", attendu)
		}
	}
	if strings.Contains(pqs, "data-ouvrir") {
		t.Error("bloc Paquets ouvert sans alerte de sécurité en cours")
	}
	if strings.Index(pqs, "openssl") > strings.Index(pqs, "curl") {
		t.Error("paquets de sécurité pas en premier")
	}
	// Page en onglets (SPEC § 66) : « En ce moment » dans « Vue d'ensemble »,
	// Paquets et Services dans « Activité ».
	vue, act := strings.Index(corps, `data-panneau-onglet="vue"`), strings.Index(corps, `data-panneau-onglet="activite"`)
	if vue < 0 || act < 0 || !(vue < strings.Index(corps, `id="maintenant"`) && strings.Index(corps, `id="maintenant"`) < act) ||
		strings.Index(corps, `id="services"`) < act || strings.Index(corps, `id="paquets"`) < act {
		t.Error("blocs pas dans leur onglet")
	}
	if !strings.Contains(corps, `data-onglets-panneaux="machine"`) {
		t.Error("barre d'onglets absente")
	}
}

// blocDetails : texte d'un <details> repérable par motif, jusqu'à sa fin.
func blocDetails(s, motif string) string {
	i := strings.Index(s, motif)
	if i < 0 {
		return "(absent)"
	}
	if j := strings.Index(s[i:], "</details>"); j >= 0 {
		return s[i : i+j]
	}
	return s[i:]
}

// Panneau de détail au clic (SPEC § 42.4) : fragment pour la page (en-tête
// X-Requested-With), page complète sinon ; adresse inconnue refusée.
func TestPanneauDetail(t *testing.T) {
	b := nouveauBanc(t)
	cpu := func(n, mode string, pente float64) serieTest {
		return serieTest{[]string{"__name__", "node_cpu_seconds_total", "instance", "web1", "cpu", n, "mode", mode}, func(k int) float64 { return float64(k) * pente }}
	}
	b.metriquesSimulees(t, []serieTest{
		// cœur 0 : 25 % utilisé ; cœur 1 : 75 % ; cœur 10 trié après 2.
		cpu("0", "idle", 22.5), cpu("0", "user", 7.5),
		cpu("1", "idle", 7.5), cpu("1", "user", 15), cpu("1", "iowait", 7.5),
		cpu("2", "idle", 30), cpu("10", "idle", 30),
		{[]string{"__name__", "node_load5", "instance", "web1"}, constante(2)},
		{[]string{"__name__", "node_memory_MemTotal_bytes", "instance", "web1"}, constante(8 << 30)},
		{[]string{"__name__", "node_memory_MemAvailable_bytes", "instance", "web1"}, constante(2 << 30)},
		{[]string{"__name__", "node_memory_Cached_bytes", "instance", "web1"}, constante(1 << 30)},
		{[]string{"__name__", "node_memory_Buffers_bytes", "instance", "web1"}, constante(0)},
		{[]string{"__name__", "node_network_receive_bytes_total", "instance", "web1", "device", "eth0"}, func(k int) float64 { return float64(k) * 30000 }},
		{[]string{"__name__", "node_network_transmit_bytes_total", "instance", "web1", "device", "eth0"}, func(k int) float64 { return float64(k) * 3000 }},
		{[]string{"__name__", "node_network_receive_bytes_total", "instance", "web1", "device", "veth12ab"}, func(k int) float64 { return float64(k) }},
	})
	b.machinesApprouvees(t, "web1")
	cl, _ := b.premierAdmin("admin", mdpAdmin)

	// Tuiles cliquables sur la page machine.
	_, corpsM, _ := b.get(cl, "/machines/web1")
	if !strings.Contains(corpsM, `<a class="ecm-case" href="/machines/web1/detail/cpu" data-panneau`) {
		t.Error("tuile CPU non cliquable")
	}
	// Réseau de « En ce moment » : cartes physiques seulement (veth exclue).
	if !strings.Contains(corpsM, `data-direct-val="net">↓ 1000 o/s</b>`) || !strings.Contains(corpsM, "↑ 100 o/s") {
		t.Error("débit réseau de « En ce moment » absent ou faux")
	}
	fragment := func(chemin string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, b.srv.URL+chemin, nil)
		req.Header.Set("X-Requested-With", "mirador")
		r, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		corps, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(corps)
	}
	code, f := fragment("/machines/web1/detail/cpu")
	if code != 200 || strings.Contains(f, "<html") {
		t.Fatalf("fragment CPU : code %d, page complète ? %s", code, f)
	}
	for _, attendu := range []string{"Processeur · web1", "4 cœurs", "Cœur 0", "25,0 %", "75,0 %", "Attente des disques (iowait)", "charge 2"} {
		if !strings.Contains(f, attendu) {
			t.Errorf("fragment CPU : %q absent", attendu)
		}
	}
	if strings.Index(f, "Cœur 2") > strings.Index(f, "Cœur 10") {
		t.Error("cœurs mal triés (10 avant 2)")
	}
	_, f = fragment("/machines/web1/detail/memoire")
	for _, attendu := range []string{"Mémoire vive (8,0 Gio)", "Utilisée par les programmes", "6,0 Gio", "75,0 % utilisée"} {
		if !strings.Contains(f, attendu) {
			t.Errorf("fragment Mémoire : %q absent", attendu)
		}
	}
	_, f = fragment("/machines/web1/detail/reseau")
	if !strings.Contains(f, "eth0") || strings.Contains(f, "veth12ab") || !strings.Contains(f, "8,0 kbit/s · 800,0 bit/s") {
		t.Errorf("fragment Réseau : %s", f)
	}
	// Sans l'en-tête : page complète avec retour.
	if code, corps, _ := b.get(cl, "/machines/web1/detail/disques"); code != 200 || !strings.Contains(corps, "<html") || !strings.Contains(corps, "Retour à web1") {
		t.Errorf("page complète : code %d", code)
	}
	for _, chemin := range []string{"/machines/web1/detail/processus", "/machines/inconnue/detail/cpu"} {
		if code, _, _ := b.get(cl, chemin); code != http.StatusNotFound {
			t.Errorf("%s : code %d, 404 attendu", chemin, code)
		}
	}
}

// Colonnes Site et Type du tableau du parc (retour du 19/11, SPEC § 71) :
// cachées d'office s'il n'y a qu'un site (ou qu'un type), triables.
func TestColonnesSiteType(t *testing.T) {
	if c := colonnesParcCachees([]ligneParc{{Site: "maison", Env: "vm"}, {Site: "maison", Env: "physique"}}); c != "site" {
		t.Errorf("un site, deux types : %q", c)
	}
	if c := colonnesParcCachees([]ligneParc{{Site: "maison", Env: "vm"}, {Site: "bureau", Env: "physique"}}); c != "" {
		t.Errorf("deux sites, deux types : %q", c)
	}
	if c := colonnesParcCachees([]ligneParc{{Site: "maison", Env: "vm"}}); c != "site,type" {
		t.Errorf("une machine : %q", c)
	}
	b := nouveauBanc(t)
	b.machinesApprouvees(t, "web1", "nas")
	_, _ = b.c.db.Exec(`UPDATE machines SET site = 'maison', env = CASE nom WHEN 'web1' THEN 'vm' ELSE 'physique' END`)
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/")
	for _, attendu := range []string{`data-colonnes-cachees="site"`, `data-tri="site">Site</th>`, `data-tri="type">Type</th>`,
		`data-site="maison" data-type="vm"`, `<td class="col-type masque-tel">Physique</td>`, `data-colonne="site"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("vue d'ensemble : %q absent", attendu)
		}
	}
}

// Chaque colonne du choix « Colonnes » a sa règle de masquage (retour du
// 19/11 : « Mises à jour » décochée restait visible).
func TestColonnesParcToutesMasquables(t *testing.T) {
	css, err := fichiers.ReadFile("statique/mirador.css")
	if err != nil {
		t.Fatal(err)
	}
	for c := range fonctions()["colonnesParc"].(func() map[string]string)() {
		if !strings.Contains(string(css), "table.sans-"+c+" .col-"+c) {
			t.Errorf("colonne %q : pas de règle « table.sans-%s .col-%s »", c, c, c)
		}
	}
}
