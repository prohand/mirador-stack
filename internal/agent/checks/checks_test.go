// SPDX-License-Identifier: GPL-3.0-or-later

package checks

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

func faire(t *testing.T, c config.Check) resultat {
	t.Helper()
	if err := c.Valider(); err != nil {
		t.Fatalf("check invalide : %v", err)
	}
	m := Nouveau([]config.Check{c}, "/proc")
	ctx, fin := context.WithTimeout(context.Background(), c.DelaiEffectif())
	defer fin()
	return m.executer(ctx, c)
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ko":
			w.WriteHeader(http.StatusInternalServerError)
		case "/loin":
			http.Redirect(w, r, "/loin", http.StatusFound) // boucle de redirections
		default:
			_, _ = w.Write([]byte("tout va bien"))
		}
	}))
	defer srv.Close()
	if r := faire(t, config.Check{Type: "http", Nom: "ok", Cible: srv.URL + "/", Contient: "va bien"}); !r.ok || r.valeur["mirador_check_http_code"] != 200 {
		t.Errorf("http ok : %+v", r)
	}
	if r := faire(t, config.Check{Type: "http", Nom: "texte", Cible: srv.URL + "/", Contient: "panne"}); r.ok {
		t.Error("texte attendu absent : ok")
	}
	if r := faire(t, config.Check{Type: "http", Nom: "ko", Cible: srv.URL + "/ko"}); r.ok || r.valeur["mirador_check_http_code"] != 500 {
		t.Errorf("http 500 : %+v", r)
	}
	if r := faire(t, config.Check{Type: "http", Nom: "boucle", Cible: srv.URL + "/loin"}); r.ok {
		t.Error("boucle de redirections : ok")
	}
	// Certificat maison : refusé par défaut, accepté avec verifier_tls = non,
	// date de fin mesurée.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer tlsSrv.Close()
	if r := faire(t, config.Check{Type: "http", Nom: "tls", Cible: tlsSrv.URL}); r.ok {
		t.Error("certificat maison accepté sans verifier_tls = non")
	}
	non := false
	r := faire(t, config.Check{Type: "http", Nom: "tls2", Cible: tlsSrv.URL, VerifierTLS: &non})
	if !r.ok || r.valeur["mirador_check_certificat_fin_timestamp_seconds"] < float64(time.Now().Unix()) {
		t.Errorf("certificat maison : %+v", r)
	}
}

func TestTCPEtDNS(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adr := l.Addr().String()
	if r := faire(t, config.Check{Type: "tcp", Nom: "ouvert", Cible: adr}); !r.ok {
		t.Error("port ouvert : ko")
	}
	l.Close()
	if r := faire(t, config.Check{Type: "tcp", Nom: "ferme", Cible: adr, Delai: config.Duree(2 * time.Second)}); r.ok {
		t.Error("port fermé : ok")
	}
	if r := faire(t, config.Check{Type: "dns", Nom: "local", Cible: "localhost"}); !r.ok {
		t.Error("résolution de localhost : ko")
	}
}

func TestProcessusFichierDossier(t *testing.T) {
	proc := t.TempDir()
	for pid, comm := range map[string]string{"10": "nginx", "11": "nginx", "12": "sshd", "self": "x"} {
		_ = os.MkdirAll(filepath.Join(proc, pid), 0o755)
		_ = os.WriteFile(filepath.Join(proc, pid, "comm"), []byte(comm+"\n"), 0o644)             // #nosec G306 -- test
		_ = os.WriteFile(filepath.Join(proc, pid, "statm"), []byte("100 25 3 1 0 2 0\n"), 0o644) // #nosec G306 -- test
	}
	r := checkProcessus(proc, config.Check{Type: "processus", Nom: "web", Cible: "nginx"})
	if !r.ok || r.valeur["mirador_check_processus_nombre"] != 2 || r.valeur["mirador_check_processus_memoire_octets"] != float64(2*25*os.Getpagesize()) {
		t.Errorf("processus : %+v", r)
	}
	if r := checkProcessus(proc, config.Check{Type: "processus", Nom: "absent", Cible: "postgres"}); r.ok {
		t.Error("processus absent : ok")
	}
	// Fichier : le plus récent des sauvegardes datées.
	d := t.TempDir()
	maintenant := time.Now()
	for i, nom := range []string{"sauv-1.tar", "sauv-2.tar", "sauv-3.tar"} {
		f := filepath.Join(d, nom)
		_ = os.WriteFile(f, make([]byte, 100*(i+1)), 0o600)
		_ = os.Chtimes(f, maintenant.Add(time.Duration(-30+i*10)*time.Hour), maintenant.Add(time.Duration(-30+i*10)*time.Hour))
	}
	r = checkFichier(config.Check{Type: "fichier", Nom: "sauv", Cible: filepath.Join(d, "sauv-*.tar")}, maintenant)
	if !r.ok || r.valeur["mirador_check_fichier_taille_octets"] != 300 || int(r.valeur["mirador_check_fichier_age_secondes"]/3600+0.5) != 10 {
		t.Errorf("fichier : %+v", r)
	}
	if r := checkFichier(config.Check{Type: "fichier", Nom: "rien", Cible: filepath.Join(d, "absent-*.tar")}, maintenant); r.ok {
		t.Error("fichier absent : ok")
	}
	r = checkDossier(context.Background(), config.Check{Type: "dossier", Nom: "d", Cible: d})
	if !r.ok || r.valeur["mirador_check_dossier_fichiers"] != 3 || r.valeur["mirador_check_dossier_taille_octets"] != 600 {
		t.Errorf("dossier : %+v", r)
	}
	if r := checkDossier(context.Background(), config.Check{Type: "dossier", Nom: "x", Cible: filepath.Join(d, "absent")}); r.ok {
		t.Error("dossier absent : ok")
	}
}

// Moteur : résultats renvoyés avec leurs étiquettes, checks lents en parallèle.
func TestMoteur(t *testing.T) {
	cs := []config.Check{{Type: "tcp", Nom: "a", Cible: "127.0.0.1:1", Modele: "ports"}, {Type: "dns", Nom: "b", Cible: "localhost"}}
	m := Nouveau(cs, "/proc")
	m.faire = func(ctx context.Context, c config.Check) resultat {
		time.Sleep(50 * time.Millisecond)
		return resultat{ok: c.Nom == "b", duree: 50 * time.Millisecond, valeur: map[string]float64{"mirador_check_x": 7}}
	}
	debut := time.Now()
	m.passer(context.Background(), []int{0, 1})
	if time.Since(debut) > 90*time.Millisecond {
		t.Error("checks faits l'un après l'autre")
	}
	b := metrics.NewBatch(16, nil)
	b.Reset(0)
	m.demarre.Do(func() {}) // pas de tâche de fond dans le test
	_ = m.Collecter(b)
	vus := map[string]float64{}
	for _, s := range b.Samples {
		k := s.Name
		for _, l := range s.Labels[:s.N] {
			k += "|" + l.Name + "=" + l.Value
		}
		vus[k] = s.Value
	}
	if vus["mirador_check_ok|type=tcp|nom=a|modele=ports|cible=127.0.0.1:1"] != 0 || vus["mirador_check_ok|type=dns|nom=b|modele=|cible=localhost"] != 1 ||
		vus["mirador_check_x|type=dns|nom=b|modele=|cible=localhost"] != 7 || len(vus) != 6 {
		t.Errorf("points : %v", vus)
	}
}
