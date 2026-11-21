// SPDX-License-Identifier: GPL-3.0-or-later

package unifi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// Relecture du 21/11 : grande réponse décodée en flux (plus ~4 fois sa
// taille en mémoire).
func TestGrandeReponseEnFlux(t *testing.T) {
	var corps []byte
	corps = append(corps, `{"data":[`...)
	for i := 0; i < 4000; i++ {
		if i > 0 {
			corps = append(corps, ',')
		}
		corps = append(corps, `{"name":"client","mac":"aa:bb:cc:dd:ee:ff","is_wired":false,"essid":"maison","ap_mac":"11:22:33:44:55:66","radio":"na","signal":-60,"satisfaction":98,"uptime":1234,"rx_bytes":123456789,"tx_bytes":987654321,"bourrage":"`...)
		for j := 0; j < 2000; j++ {
			corps = append(corps, 'x')
		}
		corps = append(corps, `"}`...)
	}
	corps = append(corps, `]}`...)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxy/network/api/s/default/stat/sta" {
			_, _ = w.Write(corps)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	h := sha256.Sum256(srv.Certificate().Raw)
	cfg := config.UniFi{Machine: "udm", Adresse: srv.URL, CleAPI: "0123456789abcdef0123", Sites: []string{"default"}, Empreinte: hex.EncodeToString(h[:])}
	var pic atomic.Uint64
	fini := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-fini:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapInuse > pic.Load() {
				pic.Store(m.HeapInuse)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	b := metrics.NewBatch(16, nil)
	b.Reset(0)
	err := Releve(context.Background(), cfg, b)
	close(fini)
	// Avant : ~4 fois la taille de la réponse (35 Mio pour 8,5). Maintenant
	// moins de 1,5 fois (déchets pas encore ramassés compris).
	if hausse := float64(pic.Load()) - float64(m0.HeapInuse); err != nil || hausse > 1.5*float64(len(corps)) {
		t.Errorf("réponse %.1f Mio : err=%v, hausse du tas %.1f Mio", float64(len(corps))/(1<<20), err, hausse/(1<<20))
	}
}
