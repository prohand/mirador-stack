// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mirador-stack/internal/agent/metrics"
)

// Réponses d'exemple de l'API Docker (champs réels, abrégés). Le conteneur
// « web » a une variable d'environnement secrète : elle ne doit jamais
// ressortir dans les métriques.
var reponsesDocker = map[string]string{
	"/containers/json": `[
	  {"Id":"aaaaaaaaaaaaaaaa","Names":["/web"],"State":"running","Labels":{"com.docker.compose.project":"site"}},
	  {"Id":"bbbbbbbbbbbbbbbb","Names":["/sauvegarde"],"State":"exited","Labels":{}},
	  {"Id":"cccccccccccccccc","Names":["/base"],"State":"exited","Labels":{"com.docker.compose.project":"site"}}
	]`,
	"/containers/aaaaaaaaaaaaaaaa/json": `{"RestartCount":2,"HostConfig":{"RestartPolicy":{"Name":"unless-stopped"}},
	  "Config":{"Env":["MOT_DE_PASSE=secret-ne-pas-envoyer"]},
	  "State":{"OOMKilled":false,"Health":{"Status":"healthy"}}}`,
	"/containers/bbbbbbbbbbbbbbbb/json": `{"RestartCount":0,"HostConfig":{"RestartPolicy":{"Name":""}},"State":{"OOMKilled":false}}`,
	"/containers/cccccccccccccccc/json": `{"RestartCount":5,"HostConfig":{"RestartPolicy":{"Name":"always"}},"State":{"OOMKilled":true,"Health":{"Status":"starting"}}}`,
	"/containers/aaaaaaaaaaaaaaaa/stats": `{"cpu_stats":{"cpu_usage":{"total_usage":12500000000}},
	  "memory_stats":{"usage":104857600,"limit":536870912,"stats":{"inactive_file":20971520}}}`,
}

func serveurDocker(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("méthode %s interdite : l'agent ne doit que lire", r.Method)
		}
		corps, ok := reponsesDocker[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(corps))
	}))
}

func TestDocker(t *testing.T) {
	srv := serveurDocker(t)
	defer srv.Close()
	d, err := NewDocker(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := collecter(t, d)
	verifier(t, p, map[string]float64{
		"node_docker_up": 1,
		"node_docker_container_running|name=web|project=site|state=running|restart_policy=unless-stopped": 1,
		"node_docker_container_running|name=sauvegarde|project=|state=exited|restart_policy=no":           0,
		"node_docker_container_running|name=base|project=site|state=exited|restart_policy=always":         0,
		"node_docker_container_restart_count|name=base|project=site":                                      5,
		"node_docker_container_oom_killed|name=base|project=site":                                         1,
		"node_docker_container_healthy|name=web|project=site":                                             1,
		"node_docker_container_cpu_seconds_total|name=web|project=site":                                   12.5,
		"node_docker_container_memory_bytes|name=web|project=site":                                        (100 - 20) << 20,
		"node_docker_container_memory_limit_bytes|name=web|project=site":                                  512 << 20,
	})
	for k := range p {
		if strings.Contains(k, "secret") || strings.Contains(k, "MOT_DE_PASSE") {
			t.Fatalf("une variable d'environnement a fuité : %s", k)
		}
		// « starting » n'est ni sain ni malade : pas de série.
		if strings.HasPrefix(k, "node_docker_container_healthy|name=base") {
			t.Errorf("healthcheck « starting » ne doit pas produire de série : %s", k)
		}
	}
}

func TestDockerInjoignable(t *testing.T) {
	d, err := NewDocker("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	b := metrics.NewBatch(4, nil)
	b.Reset(0)
	if err := d.Collecter(b); err == nil {
		t.Error("une erreur était attendue")
	}
	if b.Len() != 1 || b.Samples[0].Name != "node_docker_up" || b.Samples[0].Value != 0 {
		t.Errorf("node_docker_up=0 attendu, obtenu %+v", b.Samples)
	}
}

func TestDockerSocketUnix(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", chemin)
	if err != nil {
		t.Skip("socket Unix indisponible :", err)
	}
	srv := httptest.NewUnstartedServer(serveurDocker(t).Config.Handler)
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	d, err := NewDocker("unix://" + chemin)
	if err != nil {
		t.Fatal(err)
	}
	if p := collecter(t, d); p["node_docker_up"] != 1 {
		t.Error("lecture par socket Unix en échec")
	}
}
