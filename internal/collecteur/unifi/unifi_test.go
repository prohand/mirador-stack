// SPDX-License-Identifier: GPL-3.0-or-later

package unifi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

const (
	appareils = `{"data":[{"name":"Borne salon","mac":"aa:01","type":"uap","model":"U6LR","version":"6.6.77","ip":"192.168.1.30","state":1,
"uptime":86400,"upgradable":true,"user-num_sta":7,"guest-num_sta":1,"rx_bytes":1000,"tx_bytes":2000,"system-stats":{"cpu":"12.5","mem":"40"},
"radio_table_stats":[{"name":"wifi0","radio":"ng","cu_total":35}],"general_temperature":52},
{"name":"Switch bureau","mac":"aa:02","type":"usw","model":"USL8LP","version":"7.0","ip":"192.168.1.31","state":1,"uptime":3600,
"system-stats":{"cpu":3,"mem":20},"temperatures":[{"name":"CPU","type":"cpu","value":61}],
"port_table":[{"port_idx":1,"name":"Port 1","rx_bytes":5,"tx_bytes":6,"rx_errors":2,"speed":1000,"poe_power":"3.5"}]},
{"name":"Borne garage","mac":"aa:03","type":"uap","state":0}]}`
	clients = `{"data":[{"name":"Téléphone","mac":"bb:01","is_wired":false,"essid":"Maison","ap_mac":"aa:01","radio":"na","signal":-61,
"satisfaction":96,"uptime":1200,"rx_bytes":10,"tx_bytes":20},{"hostname":"pc-bureau","mac":"bb:02","is_wired":true,"uptime":99}]}`
	sante = `{"data":[{"subsystem":"wlan","num_disconnected":1},{"subsystem":"lan","num_disconnected":0}]}`
)

// controleur : faux contrôleur ; unifiOS : chemins /proxy/network.
func controleur(t *testing.T, unifiOS bool) *httptest.Server {
	p := ""
	if unifiOS {
		p = "/proxy/network"
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connecte := r.Header.Get("X-API-KEY") == "cle-api-de-test-0123456789"
		if c, err := r.Cookie("session"); err == nil && c.Value == "ok" {
			connecte = true
		}
		switch {
		case r.Method == http.MethodPost && (unifiOS && r.URL.Path == "/api/auth/login" || !unifiOS && r.URL.Path == "/api/login"):
			var corps strings.Builder
			buf := make([]byte, 512)
			n, _ := r.Body.Read(buf)
			corps.Write(buf[:n])
			if !strings.Contains(corps.String(), `"password":"mot-de-passe"`) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "ok", Path: "/"})
			w.Header().Set("X-Csrf-Token", "jeton-csrf")
		case r.Method == http.MethodGet && !connecte:
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == p+"/api/self/sites":
			_, _ = w.Write([]byte(`{"data":[{"name":"default","desc":"Maison"}]}`))
		case r.URL.Path == p+"/api/s/default/stat/device":
			_, _ = w.Write([]byte(appareils))
		case r.URL.Path == p+"/api/s/default/stat/sta":
			_, _ = w.Write([]byte(clients))
		case r.URL.Path == p+"/api/s/default/stat/health":
			_, _ = w.Write([]byte(sante))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func empreinte(srv *httptest.Server) string {
	h := sha256.Sum256(srv.Certificate().Raw)
	return hex.EncodeToString(h[:])
}

func points(b *metrics.Batch) map[string]float64 {
	out := map[string]float64{}
	for _, s := range b.Samples[:b.Len()] {
		k := s.Name
		for _, l := range s.Labels[:s.N] {
			k += "|" + l.Name + "=" + l.Value
		}
		out[k] = s.Value
	}
	return out
}

func TestReleveUniFi(t *testing.T) {
	for _, cas := range []struct {
		nom     string
		unifiOS bool
		cfg     config.UniFi
	}{
		{"UniFi OS, compte local", true, config.UniFi{Utilisateur: "lecture", MotDePasse: "mot-de-passe"}},
		{"UniFi OS, clé API", true, config.UniFi{CleAPI: "cle-api-de-test-0123456789"}},
		{"contrôleur classique", false, config.UniFi{Utilisateur: "lecture", MotDePasse: "mot-de-passe"}},
	} {
		srv := controleur(t, cas.unifiOS)
		cfg := cas.cfg
		cfg.Machine, cfg.Adresse, cfg.Empreinte = "unifi", srv.URL, empreinte(srv)
		b := metrics.NewBatch(64, nil)
		b.Reset(0)
		if err := Releve(context.Background(), cfg, b); err != nil {
			t.Fatalf("%s : %v", cas.nom, err)
		}
		p := points(b)
		attendus := map[string]float64{
			"unpoller_device_info|name=Borne salon|type=uap|site_name=default|model=U6LR|version=6.6.77|ip=192.168.1.30|mac=aa:01":                1,
			"unpoller_device_cpu_utilization_ratio|name=Borne salon|type=uap|site_name=default":                                                   0.125,
			"unpoller_device_uptime_seconds|name=Switch bureau|type=usw|site_name=default":                                                        3600,
			"unpoller_device_upgradeable|name=Borne salon|type=uap|site_name=default":                                                             1,
			"unpoller_device_stations|name=Borne salon|type=uap|site_name=default|station_type=user":                                              7,
			"unpoller_device_temperature_celsius|name=Switch bureau|type=usw|site_name=default|temp_area=CPU|temp_type=cpu":                       61,
			"unpoller_device_temperature_celsius|name=Borne salon|type=uap|site_name=default|temp_area=general|temp_type=board":                   52,
			"unpoller_device_port_receive_errors_total|name=Switch bureau|type=usw|site_name=default|port_num=1|port_name=Port 1":                 2,
			"unpoller_device_port_port_speed_bps|name=Switch bureau|type=usw|site_name=default|port_num=1|port_name=Port 1":                       1e9,
			"unpoller_device_port_poe_watts|name=Switch bureau|type=usw|site_name=default|port_num=1|port_name=Port 1":                            3.5,
			"unpoller_device_radio_channel_utilization_total_ratio|name=Borne salon|type=uap|site_name=default|radio=ng":                          0.35,
			"unpoller_client_radio_signal_db|name=Téléphone|mac=bb:01|site_name=default|wired=false|essid=Maison|ap_name=Borne salon|radio=na":    -61,
			"unpoller_client_satisfaction_ratio|name=Téléphone|mac=bb:01|site_name=default|wired=false|essid=Maison|ap_name=Borne salon|radio=na": 0.96,
			"unpoller_client_uptime_seconds|name=pc-bureau|mac=bb:02|site_name=default|wired=true|essid=|ap_name=|radio=":                         99,
			"unpoller_site_disconnected|site_name=default|subsystem=wlan":                                                                         1,
		}
		for k, v := range attendus {
			if got, ok := p[k]; !ok || got != v {
				t.Errorf("%s : %s = %v (présent %v)", cas.nom, k, got, ok)
			}
		}
		// Borne déconnectée : seulement son info.
		if _, ok := p["unpoller_device_uptime_seconds|name=Borne garage|type=uap|site_name=default"]; ok {
			t.Errorf("%s : mesures d'une borne déconnectée", cas.nom)
		}
	}
}

func TestRefusUniFi(t *testing.T) {
	srv := controleur(t, true)
	b := metrics.NewBatch(8, nil)
	b.Reset(0)
	base := config.UniFi{Machine: "unifi", Adresse: srv.URL, Utilisateur: "lecture", MotDePasse: "mot-de-passe"}
	// Certificat fait maison sans empreinte : refusé (vérifié).
	if err := Releve(context.Background(), base, b); err == nil {
		t.Error("certificat non vérifié accepté")
	}
	// Mauvaise empreinte : refusé.
	c := base
	c.Empreinte = strings.Repeat("0", 64)
	if err := Releve(context.Background(), c, b); err == nil {
		t.Error("mauvaise empreinte acceptée")
	}
	// Mauvais mot de passe.
	c = base
	c.Empreinte, c.MotDePasse = empreinte(srv), "faux"
	if err := Releve(context.Background(), c, b); err == nil || !strings.Contains(err.Error(), "refusé") {
		t.Errorf("mauvais mot de passe : %v", err)
	}
	// Secret encore chiffré.
	c.MotDePasse = config.PrefixeSecretChiffre + "x"
	if err := Releve(context.Background(), c, b); err == nil {
		t.Error("secret chiffré accepté")
	}
}

// BenchmarkAjouter : décodage d'un site (3 appareils, 2 clients) et
// métriques, hors réseau.
//
//	go test ./internal/collecteur/unifi/ -bench Ajouter -benchmem -run '^$'
func BenchmarkAjouter(b *testing.B) {
	lot := metrics.NewBatch(256, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var d Donnees
		var e struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal([]byte(appareils), &e)
		_ = json.Unmarshal(e.Data, &d.Appareils)
		_ = json.Unmarshal([]byte(clients), &e)
		_ = json.Unmarshal(e.Data, &d.Clients)
		lot.Reset(0)
		Ajouter(lot, "default", d)
	}
}
