// SPDX-License-Identifier: GPL-3.0-or-later

package changements

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
)

// fauxLecteur : réponses figées par expression.
type fauxLecteur map[string][]alertes.Echantillon

func (f fauxLecteur) Instant(_ context.Context, expr string, _ time.Time) ([]alertes.Echantillon, error) {
	return f[expr], nil
}

func ech(v float64, kv ...string) alertes.Echantillon {
	return alertes.Echantillon{Etiquettes: labels.FromStrings(kv...), Valeur: v}
}

const (
	eNoyau   = `max by (instance, release) (node_uname_info)`
	eBoot    = `max by (instance) (node_boot_time_seconds)`
	eAgent   = `max by (instance, version) (timestamp(mirador_agent_info))`
	eRedem   = `max by (instance, name) (node_docker_container_restart_count)`
	eImage   = `max by (instance, name, image) (node_docker_container_info)`
	eDebut   = `max by (instance, name) (node_docker_container_started_timestamp_seconds)`
	ePaquets = `max by (instance, package, from, to) (apt_package_installed)`
)

func TestDetecteur(t *testing.T) {
	ctx := context.Background()
	d := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(d, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := base.Migrer(ctx, db, filepath.Join(d, "copies")); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	l := fauxLecteur{
		eNoyau: {ech(1, "instance", "pve1", "release", "6.8.12-4-pve")},
		eBoot:  {ech(float64(t0.Add(-48*time.Hour).Unix()), "instance", "pve1")},
		eAgent: {ech(1, "instance", "pve1", "version", "0.2.31")},
		eRedem: {ech(0, "instance", "pve1", "name", "ha"), ech(3, "instance", "pve1", "name", "z2m")},
		eImage: {ech(1, "instance", "pve1", "name", "ha", "image", "ghcr.io/home-assistant/home-assistant:2026.11.1")},
		eDebut: {ech(float64(t0.Add(-time.Hour).Unix()), "instance", "pve1", "name", "ha"),
			ech(float64(t0.Add(-time.Hour).Unix()), "instance", "pve1", "name", "z2m")},
		ePaquets: {ech(float64(t0.Add(-24*time.Hour).Unix()), "instance", "pve1", "package", "vieux", "from", "1", "to", "2")},
	}
	det := &Detecteur{DB: db, Lecteur: l}
	if err := det.Tour(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if cs, _ := Lister(ctx, db, "pve1", time.Time{}, 50); len(cs) != 0 {
		t.Fatalf("premier tour : rien à signaler : %+v", cs)
	}

	// Mise à jour de nuit : paquets, redémarrage (nouveau noyau), conteneur
	// recréé, z2m redémarré tout seul (plantage : rien), nouvel agent.
	l[ePaquets] = append(l[ePaquets],
		ech(float64(t0.Add(12*time.Minute).Unix()), "instance", "pve1", "package", "openssl", "from", "3.0.15-1", "to", "3.0.15-2"),
		ech(float64(t0.Add(12*time.Minute).Unix()), "instance", "pve1", "package", "jq", "from", "", "to", "1.7.1"))
	l[eNoyau] = []alertes.Echantillon{ech(1, "instance", "pve1", "release", "6.8.12-5-pve")}
	l[eBoot] = []alertes.Echantillon{ech(float64(t0.Add(19*time.Minute).Unix()), "instance", "pve1")}
	l[eAgent] = []alertes.Echantillon{ech(1, "instance", "pve1", "version", "0.2.31"), ech(2, "instance", "pve1", "version", "0.2.32")}
	l[eImage] = []alertes.Echantillon{ech(1, "instance", "pve1", "name", "ha", "image", "ghcr.io/home-assistant/home-assistant:2026.11.2")}
	l[eRedem] = []alertes.Echantillon{ech(0, "instance", "pve1", "name", "ha"), ech(4, "instance", "pve1", "name", "z2m")}
	l[eDebut] = []alertes.Echantillon{ech(float64(t0.Add(25*time.Minute).Unix()), "instance", "pve1", "name", "ha"),
		ech(float64(t0.Add(26*time.Minute).Unix()), "instance", "pve1", "name", "z2m")}
	if err := det.Tour(ctx, t0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Un paquet de plus, 5 min après : ajouté à la même ligne.
	l[ePaquets] = append(l[ePaquets], ech(float64(t0.Add(17*time.Minute).Unix()), "instance", "pve1", "package", "curl", "from", "8.5.0-1", "to", "8.5.0-2"))
	if err := det.Tour(ctx, t0.Add(31*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cs, err := Lister(ctx, db, "pve1", time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range cs {
		if _, deja := got[c.Type]; deja {
			t.Errorf("deux lignes %s : %+v", c.Type, cs)
		}
		got[c.Type] = c.Detail
	}
	attendus := map[string]string{
		Paquets:     "jq 1.7.1 (nouveau), openssl 3.0.15-1 → 3.0.15-2, curl 8.5.0-1 → 8.5.0-2",
		Redemarrage: "Machine redémarrée, noyau 6.8.12-4-pve → 6.8.12-5-pve",
		Agent:       "mirador-agent 0.2.31 → 0.2.32",
		Conteneur:   "ha recréé : image home-assistant:2026.11.1 → home-assistant:2026.11.2",
	}
	for k, v := range attendus {
		if got[k] != v {
			t.Errorf("%s : %q, attendu %q", k, got[k], v)
		}
	}
	if len(got) != len(attendus) {
		t.Errorf("changements : %+v", got)
	}
	// Message d'alerte : dernier changement dans l'heure d'avant.
	c, ok := Avant(ctx, db, "pve1", t0.Add(40*time.Minute), time.Hour)
	if !ok || c.Type != Agent { // repéré à 03:30, le plus récent
		t.Errorf("avant : %+v %v", c, ok)
	}
	if !strings.HasPrefix((Changement{Type: Paquets, Detail: "a 1 → 2, b 1 → 2"}).Resume(), "2 paquets") {
		t.Error("résumé paquets")
	}
	// Redémarrage du serveur : état relu, rien de répété.
	det2 := &Detecteur{DB: db, Lecteur: l}
	if err := det2.Tour(ctx, t0.Add(32*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if cs2, _ := Lister(ctx, db, "pve1", time.Time{}, 50); len(cs2) != len(cs) {
		t.Fatalf("après redémarrage : %d lignes au lieu de %d", len(cs2), len(cs))
	}
}

// Ports (demande du 27/11) : ouvert puis fermé sur une machine connue ;
// rien au tout premier tour ni pour une machine nouvelle ; machine muette :
// ses ports ne sont pas « fermés ».
func TestPorts(t *testing.T) {
	ctx := context.Background()
	d := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(d, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := base.Migrer(ctx, db, filepath.Join(d, "copies")); err != nil {
		t.Fatal(err)
	}
	const ePorts = `max by (instance, port, adresse, protocole) (last_over_time(mirador_port_ecoute[10m]))`
	t0 := time.Date(2026, 11, 27, 21, 0, 0, 0, time.UTC)
	l := fauxLecteur{
		eBoot:  {ech(float64(t0.Add(-48*time.Hour).Unix()), "instance", "web1")},
		ePorts: {ech(1, "instance", "web1", "port", "22", "adresse", "*", "protocole", "ssh")},
	}
	det := &Detecteur{DB: db, Lecteur: l}
	tour := func(dt time.Duration) []Changement {
		if err := det.Tour(ctx, t0.Add(dt)); err != nil {
			t.Fatal(err)
		}
		cs, _ := ListerType(ctx, db, Port, t0.Add(-time.Hour), 50)
		return cs
	}
	if cs := tour(0); len(cs) != 0 {
		t.Fatalf("premier tour : %+v", cs)
	}
	l[ePorts] = append(l[ePorts], ech(1, "instance", "web1", "port", "8080", "adresse", "*", "protocole", "http"),
		ech(1, "instance", "neuve", "port", "443", "adresse", "*", "protocole", "https"))
	l[eBoot] = append(l[eBoot], ech(float64(t0.Unix()), "instance", "neuve"))
	cs := tour(time.Minute)
	if len(cs) != 1 || cs[0].Machine != "web1" || cs[0].Detail != "Port ouvert : tcp/8080 (http) · toutes les adresses" {
		t.Fatalf("port ouvert : %+v", cs)
	}
	// Protocole relu autrement : pas un changement.
	l[ePorts][1] = ech(1, "instance", "web1", "port", "8080", "adresse", "*", "protocole", "https")
	if cs := tour(2 * time.Minute); len(cs) != 1 {
		t.Fatalf("protocole changé : %+v", cs)
	}
	// 8080 fermé ; « neuve » muette : rien pour elle.
	l[ePorts] = l[ePorts][:1]
	cs = tour(3 * time.Minute)
	if len(cs) != 2 || cs[0].Detail != "Port fermé : tcp/8080 · toutes les adresses" {
		t.Fatalf("port fermé : %+v", cs)
	}
	for _, c := range cs {
		if c.Machine == "neuve" {
			t.Errorf("machine muette signalée : %+v", c)
		}
	}
	if cs := tour(4 * time.Minute); len(cs) != 2 {
		t.Errorf("« fermé » redit : %+v", cs)
	}
}
