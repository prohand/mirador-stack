// SPDX-License-Identifier: GPL-3.0-or-later

package unifi

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"
)

// Relecture du 21/11 : grande réponse lue élément par élément. Avant, la
// réponse entière était lue, copiée puis décodée : plus de 3 fois sa
// taille allouée (35 Mio de pointe pour 8,5 Mio). Mesure : total alloué
// par le décodage seul (indépendant du moment où Go fait son ménage).
func TestGrandeReponseEnFlux(t *testing.T) {
	var corps []byte
	corps = append(corps, `{"meta":{"rc":"ok"},"data":[`...)
	for i := 0; i < 4000; i++ {
		if i > 0 {
			corps = append(corps, ',')
		}
		corps = append(corps, `{"name":"client","mac":"aa:bb:cc:dd:ee:ff","is_wired":false,"essid":"maison","signal":-60,"satisfaction":98,"rx_bytes":123456789,"bourrage":"`...)
		corps = append(corps, bytes.Repeat([]byte("x"), 2000)...)
		corps = append(corps, `"}`...)
	}
	corps = append(corps, `]}`...)
	var clients []ClientUniFi
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	err := lireData(json.NewDecoder(bytes.NewReader(corps)), &clients)
	runtime.ReadMemStats(&m1)
	if err != nil || len(clients) != 4000 || clients[3999].Signal != -60 {
		t.Fatalf("err=%v, %d clients", err, len(clients))
	}
	if alloue := m1.TotalAlloc - m0.TotalAlloc; alloue > uint64(len(corps)) {
		t.Errorf("réponse %.1f Mio : %.1f Mio alloués pour la décoder", float64(len(corps))/(1<<20), float64(alloue)/(1<<20))
	}
}
