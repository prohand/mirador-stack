// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/taches"
)

var reURLPing = regexp.MustCompile(`<code id="url-ping">([^<]+)</code>`)

// Tâches planifiées (SPEC § 80) : création d'une tâche « ping » dans la
// page, signal sans session (même hors de la liste d'adresses), états.
func TestTachesPing(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/taches")
	code, corps, _ := b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"creer"}, "nom": {"Sauvegarde NAS"},
		"periode": {"86400"}, "marge": {"1"}, "gravite": {"critical"}})
	if code != http.StatusOK {
		t.Fatalf("création : %d", code)
	}
	adresse := trouver(t, reURLPing, corps)
	chemin := adresse[strings.Index(adresse, "/ping/"):]
	// Liste d'adresses restreinte : /ping passe quand même (jeton).
	sec := SecuriteDefaut()
	sec.IPs = "203.0.113.0/24"
	b.c.securite.Store(&sec)
	anonyme := b.client()
	if code, corps, _ := b.get(anonyme, chemin+"/code/3?duree=120&message=disque%20plein"); code != http.StatusOK || corps != "OK\n" {
		t.Fatalf("ping : %d %q", code, corps)
	}
	if code, _, _ := b.get(anonyme, "/ping/jeton-inconnu-jeton-inconnu"); code != http.StatusNotFound {
		t.Fatalf("jeton inconnu : %d", code)
	}
	if code, _, _ := b.get(anonyme, chemin+"/code/999"); code != http.StatusBadRequest {
		t.Fatalf("code hors limites : %d", code)
	}
	if code, _, _ := b.get(anonyme, "/"); code != http.StatusForbidden {
		t.Fatalf("le reste de la console doit rester fermé : %d", code)
	}
	b.c.securite.Store(func() *Securite { s := SecuriteDefaut(); return &s }())
	_, corps, _ = b.get(c, "/taches")
	for _, attendu := range []string{"Sauvegarde NAS", "Échec · code 3", "disque plein", "2 min", "Signal (curl)"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}
	if strings.Contains(corps, chemin) {
		t.Error("le jeton ne doit plus être affiché")
	}
	// Recherche par nom ou machine (grand parc, SPEC § 141).
	if _, c1, _ := b.get(c, "/taches?q=nas"); !strings.Contains(c1, "Échec · code 3") || !strings.Contains(c1, `value="nas"`) {
		t.Error("recherche : tâche attendue absente")
	}
	if _, c2, _ := b.get(c, "/taches?q=introuvable"); strings.Contains(c2, "Échec · code 3") {
		t.Error("recherche : tâche non filtrée")
	}
	// Limite : 60 appels par minute et par adresse.
	n := 0
	for i := 0; i < 70; i++ {
		if code, _, _ := b.get(anonyme, chemin); code == http.StatusTooManyRequests {
			n++
		}
	}
	if n == 0 {
		t.Fatal("pas de limite d'appels")
	}
	// Bouton « Ajouter » : formulaire en haut de page, champ Nom prêt
	// (retour du 20/11 : formulaire en bas, le bouton semblait sans effet).
	_, page0, _ := b.get(c, "/taches")
	if !strings.Contains(page0, `href="/taches?ajout=1#nouvelle"`) || strings.Contains(page0, `id="nouvelle"`) {
		t.Error("bouton Ajouter : lien ou formulaire fermé attendu")
	}
	_, page0, _ = b.get(c, "/taches?ajout=1")
	if i, j := strings.Index(page0, `id="nouvelle"`), strings.Index(page0, "<h2>Suivies</h2>"); i < 0 || j < 0 || i > j || !strings.Contains(page0, "autofocus") {
		t.Error("formulaire d'ajout : absent, ou pas en haut de page")
	}
	// Entrée de menu à part (retour du 20/11), plus d'onglet dans Sondes.
	_, page, _ := b.get(c, "/taches")
	if !strings.Contains(page, `data-cle="taches"`) || !strings.Contains(page, `href="/taches" title="Tâches planifiées" class="`) {
		t.Error("entrée « Tâches planifiées » absente du menu")
	}
	if _, page, _ = b.get(c, "/sondes"); strings.Contains(page, `<a href="/taches">Tâches planifiées</a></p>`) {
		t.Error("onglet Tâches encore dans Sondes")
	}
}

// Les expressions des règles TacheEnEchec / TacheEnRetard sur de vraies
// séries : échec d'une tâche suivie seulement ; échéance passée.
func TestReglesTachesSeries(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(context.Background())
	ajout := func(v float64, l ...string) { _, _ = app.Append(0, labels.FromStrings(l...), m.UnixMilli(), v) }
	ajout(3, "__name__", "mirador_tache_code", "instance", "web1", "job", "node", "source", "systemd", "tache", "sauvegarde")
	ajout(2, "__name__", "mirador_tache_code", "instance", "web1", "job", "node", "source", "systemd", "tache", "pas-suivie")
	ajout(1, "__name__", "mirador_tache_suivie", "instance", "web1", "job", "mirador", "source", "systemd", "tache", "sauvegarde", "gravite", "warning")
	ajout(float64(m.Add(-time.Hour).Unix()), "__name__", "mirador_tache_echeance_timestamp_seconds", "instance", "web1", "job", "mirador", "source", "systemd", "tache", "sauvegarde", "gravite", "warning")
	ajout(float64(m.Add(time.Hour).Unix()), "__name__", "mirador_tache_echeance_timestamp_seconds", "instance", "nas", "job", "mirador", "source", "ping", "tache", "a-l-heure", "gravite", "warning")
	_ = app.Commit()
	l := alertes.NouveauLecteur(db, nil)
	for _, r := range alertes.ReglesMirador() {
		if r.Nom != "TacheEnEchec" && r.Nom != "TacheEnRetard" {
			continue
		}
		ech, err := l.Instant(context.Background(), r.Expr, m)
		if err != nil || len(ech) != 1 || ech[0].Etiquettes.Get("tache") != "sauvegarde" {
			t.Fatalf("%s : %v %+v", r.Nom, err, ech)
		}
	}
}

// Description en clair des tâches détectées (retour du 20/11 : « à quoi
// correspond vzdump ? »).
func TestDescriptionTache(t *testing.T) {
	for _, c := range []struct{ source, nom, attendu string }{
		{"pve", "vzdump", "Sauvegarde planifiée des VM et conteneurs (travail de Datacenter › Sauvegarde, lancé sur ce nœud)"},
		{"pve", "vzdump 101", "Sauvegarde de la VM ou du conteneur 101"},
		{"pbs", "garbage_collection datastore1", "Nettoyage du datastore (garbage collection) (datastore1)"},
		{"pbs", "verificationjob", "Vérification planifiée des sauvegardes"},
		{"systemd", "sauvegarde-restic.timer", "Timer systemd : lance le service sauvegarde-restic à heure fixe"},
		{"ping", "Sauvegarde NAS", ""},
	} {
		if got := descriptionTache(c.source, c.nom); got != c.attendu {
			t.Errorf("%s %s : %q", c.source, c.nom, got)
		}
	}
}

// Prochaine exécution (retour du 20/11) : annoncée par le timer ou le
// planning, sinon estimée (dernière fin + fréquence).
func TestProchaineTache(t *testing.T) {
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if p, est := prochaineTache(taches.Releve{Prochaine: m.Add(9 * time.Hour), Fin: m.Add(-time.Hour)}, 24*time.Hour); !p.Equal(m.Add(9*time.Hour)) || est {
		t.Errorf("annoncée : %v %v", p, est)
	}
	if p, est := prochaineTache(taches.Releve{Fin: m.Add(-time.Hour)}, 24*time.Hour); !p.Equal(m.Add(23*time.Hour)) || !est {
		t.Errorf("estimée : %v %v", p, est)
	}
	if p, _ := prochaineTache(taches.Releve{}, 24*time.Hour); !p.IsZero() {
		t.Errorf("sans dernière fin : %v", p)
	}
	if prochaineTexte(m.Add(9*time.Hour), m) != "dans 9 h" || prochaineTexte(m.Add(-2*time.Hour), m) != "prévue il y a 2 h" || prochaineTexte(time.Time{}, m) != "" {
		t.Error("texte de la prochaine exécution")
	}
}

// « Voir l'erreur » (SPEC § 93) : timer en échec avec les lignes du journal
// de son service autour de la fin ; signal curl avec son message ; tâche
// inconnue : 404.
func TestErreurTache(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir(), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jx.Fermer() })
	b.c.opt.Logs = jx
	_ = jx.Ecrire(context.Background(), []stockage.Ligne{
		{T: m.Add(-3 * time.Minute).UnixMilli(), Msg: "Processing /etc/letsencrypt/renewal/exemple.fr.conf", Champs: map[string]string{"host": "web1", "unit": "certbot.service"}},
		{T: m.Add(-2 * time.Minute).UnixMilli(), Msg: "Failed to renew certificate exemple.fr with error: Some challenges have failed.", Champs: map[string]string{"host": "web1", "unit": "certbot.service"}},
		{T: m.Add(-2 * time.Minute).UnixMilli(), Msg: "autre service", Champs: map[string]string{"host": "web1", "unit": "nginx.service"}},
	})
	b.metriquesSimulees(t, []serieTest{
		{[]string{"__name__", "mirador_tache_fin_timestamp_seconds", "instance", "web1", "job", "node", "source", "systemd", "tache", "certbot.timer"}, constante(float64(m.Add(-2 * time.Minute).Unix()))},
		{[]string{"__name__", "mirador_tache_debut_timestamp_seconds", "instance", "web1", "job", "node", "source", "systemd", "tache", "certbot.timer"}, constante(float64(m.Add(-3 * time.Minute).Unix()))},
		{[]string{"__name__", "mirador_tache_code", "instance", "web1", "job", "node", "source", "systemd", "tache", "certbot.timer"}, constante(1)},
	})
	c, _ := b.premierAdmin("admin", mdpAdmin)
	if _, page, _ := b.get(c, "/taches"); !strings.Contains(page, `href="/taches/erreur?cle=web1%7csystemd%7ccertbot.timer" data-panneau`) &&
		!strings.Contains(page, `href="/taches/erreur?cle=web1|systemd|certbot.timer" data-panneau`) {
		t.Errorf("bouton « Voir l'erreur » absent :\n%s", extrait(page, "certbot"))
	}
	_, corps, _ := b.get(c, "/taches/erreur?cle="+url.QueryEscape("web1|systemd|certbot.timer"))
	for _, attendu := range []string{"Dernières lignes du journal de certbot.service", `<span class="e">Failed to renew certificate`, "Processing /etc/letsencrypt",
		"Échec · code 1", "Ouvrir dans Logs"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("panneau : %q absent", attendu)
		}
	}
	if strings.Contains(corps, "autre service") {
		t.Error("logs d'un autre service mélangés")
	}
	// Signal curl en échec avec un message.
	csrf := b.csrf(c, "/taches")
	_, page, _ := b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"creer"}, "nom": {"Sauvegarde NAS"}, "periode": {"86400"}, "marge": {"1"}, "gravite": {"warning"}})
	adresse := trouver(t, reURLPing, page)
	anonyme := b.client()
	b.get(anonyme, adresse[strings.Index(adresse, "/ping/"):]+"/echec?message=disque%20plein")
	ts, _ := taches.Lister(context.Background(), b.db)
	_, corps, _ = b.get(c, "/taches/erreur?cle=ping:"+strconv.FormatInt(ts[0].ID, 10))
	if !strings.Contains(corps, "<h3>Message</h3><p>disque plein</p>") {
		t.Errorf("message du signal absent :\n%s", extrait(corps, "Message"))
	}
	if code, _, _ := b.get(c, "/taches/erreur?cle=inconnue"); code != http.StatusNotFound {
		t.Errorf("tâche inconnue : %d", code)
	}
}
