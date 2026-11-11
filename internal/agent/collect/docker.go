// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// Docker produit les métriques de obs-docker.py utilisées par les règles et
// dashboards (SPEC.md § 4.1) :
//
//	node_docker_up
//	node_docker_container_running{name,project,state,restart_policy}
//	node_docker_container_restart_count{name,project}
//	node_docker_container_oom_killed{name,project}
//	node_docker_container_healthy{name,project}     (absent sans healthcheck)
//	node_docker_container_cpu_seconds_total{name,project}
//	node_docker_container_memory_bytes{name,project}
//	node_docker_container_memory_limit_bytes{name,project}
//
// Accès (SPEC.md § 6.5) : uniquement par un proxy de socket Docker en lecture
// seule (tecnativa/docker-socket-proxy, CONTAINERS=1, POST=0), jamais par
// /var/run/docker.sock directement. Seuls les champs utiles des réponses sont
// décodés : les variables d'environnement des conteneurs (secrets possibles)
// sont ignorées.
type Docker struct {
	base   string // http://127.0.0.1:2375 ou http://unix (socket)
	client *http.Client
	delai  time.Duration
}

// NewDocker crée le collecteur. adresse : « http://127.0.0.1:2375 » ou
// « unix:///chemin/vers/socket » (déjà validée par la configuration).
func NewDocker(adresse string) (*Docker, error) {
	d := &Docker{delai: 20 * time.Second}
	transport := &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 2 * time.Minute}
	u, err := url.Parse(adresse)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http":
		d.base = strings.TrimRight(adresse, "/")
	case "unix":
		chemin := u.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", chemin)
		}
		d.base = "http://docker"
	default:
		return nil, fmt.Errorf("adresse Docker %q : http:// ou unix:// attendu", adresse)
	}
	d.client = &http.Client{Transport: transport, Timeout: 10 * time.Second}
	return d, nil
}

func (d *Docker) Nom() string { return "docker" }
func (d *Docker) Lent() bool  { return true }

// Champs lus dans les réponses de l'API Docker (le reste est ignoré).
type conteneurListe struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

type conteneurDetail struct {
	RestartCount int `json:"RestartCount"`
	HostConfig   struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	State struct {
		OOMKilled bool `json:"OOMKilled"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
}

type conteneurStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"` // nanosecondes
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

func (d *Docker) Collecter(b *metrics.Batch) error {
	ctx, annuler := context.WithTimeout(context.Background(), d.delai)
	defer annuler()

	var liste []conteneurListe
	if err := d.get(ctx, "/containers/json?all=1", &liste); err != nil {
		b.Add("node_docker_up", 0)
		return err
	}
	b.Add("node_docker_up", 1)

	var premiereErreur error
	for _, c := range liste {
		nom := c.ID
		if len(nom) > 12 {
			nom = nom[:12]
		}
		if len(c.Names) > 0 {
			nom = strings.TrimPrefix(c.Names[0], "/")
		}
		base := [2]metrics.Label{
			metrics.L("name", nom),
			metrics.L("project", c.Labels["com.docker.compose.project"]),
		}

		var det conteneurDetail
		if err := d.get(ctx, "/containers/"+url.PathEscape(c.ID)+"/json", &det); err != nil {
			premiereErreur = garderPremiere(premiereErreur, err)
			continue
		}
		politique := det.HostConfig.RestartPolicy.Name
		if politique == "" {
			politique = "no"
		}
		b.Add("node_docker_container_running", bool01(c.State == "running"),
			base[0], base[1], metrics.L("state", c.State), metrics.L("restart_policy", politique))
		b.Add("node_docker_container_restart_count", float64(det.RestartCount), base[:]...)
		b.Add("node_docker_container_oom_killed", bool01(det.State.OOMKilled), base[:]...)
		if h := det.State.Health; h != nil && (h.Status == "healthy" || h.Status == "unhealthy") {
			b.Add("node_docker_container_healthy", bool01(h.Status == "healthy"), base[:]...)
		}
		if c.State != "running" {
			continue
		}

		// « one-shot » : réponse immédiate, sans attendre un second échantillon.
		var st conteneurStats
		if err := d.get(ctx, "/containers/"+url.PathEscape(c.ID)+"/stats?stream=false&one-shot=true", &st); err != nil {
			premiereErreur = garderPremiere(premiereErreur, err)
			continue
		}
		b.Add("node_docker_container_cpu_seconds_total", float64(st.CPUStats.CPUUsage.TotalUsage)/1e9, base[:]...)
		if st.MemoryStats.Usage > 0 {
			// Même calcul que « docker stats » : usage moins le cache inactif
			// (inactive_file en cgroup v2, total_inactive_file en cgroup v1).
			cache, ok := st.MemoryStats.Stats["inactive_file"]
			if !ok {
				cache = st.MemoryStats.Stats["total_inactive_file"]
			}
			utilise := st.MemoryStats.Usage
			if cache < utilise {
				utilise -= cache
			} else {
				utilise = 0
			}
			b.Add("node_docker_container_memory_bytes", float64(utilise), base[:]...)
		}
		if st.MemoryStats.Limit > 0 {
			b.Add("node_docker_container_memory_limit_bytes", float64(st.MemoryStats.Limit), base[:]...)
		}
	}
	return premiereErreur
}

// tailleMaxReponse borne une réponse de l'API (protection mémoire).
const tailleMaxReponse = 4 << 20

// get fait une requête GET et décode la réponse JSON dans v.
func (d *Docker) get(ctx context.Context, chemin string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+chemin, nil)
	if err != nil {
		return err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode == http.StatusForbidden {
			return errors.New("proxy Docker : accès refusé (CONTAINERS=1 attendu)")
		}
		return fmt.Errorf("API Docker %s : HTTP %d", chemin, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, tailleMaxReponse)).Decode(v)
}

// garderPremiere garde la première erreur rencontrée.
func garderPremiere(premiere, nouvelle error) error {
	if premiere != nil {
		return premiere
	}
	return nouvelle
}
