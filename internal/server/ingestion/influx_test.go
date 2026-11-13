// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/stockage"
)

// cleSerie : nom puis étiquettes, pour comparer simplement.
func cleSerie(s stockage.Serie) string {
	var b strings.Builder
	for i, l := range s.Labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Nom + "=" + l.Valeur)
	}
	return b.String()
}

func TestDecoderInflux(t *testing.T) {
	maintenant := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	corps := strings.Join([]string{
		"# commentaire",
		`cpu,cpu=cpu-total,host=opnsense usage_idle=97.5,usage_user=1.25 1763114400000000000`,
		`mem,host=opnsense used_percent=42i,available=1024u`,
		`pf,host=opnsense entries=1234i,running=true,match="texte ignoré, avec espace"`,
		`net,host=opnsense,interface=igb0\ lan bytes_recv=10i 1763114400000000000`,
		`ma\ mesure,ta\,g=v\=1 champ\ x=1 1763114400000000000`,
		`system,host=opnsense uptime_format="3 days" 1763114400000000000`, // texte seul : rien, ligne correcte
		`mauvaise ligne`,                 // pas de champ
		`cpu,host=a usage=abc`,           // valeur invalide
		`cpu,host=a usage=NaN`,           // NaN refusé
		`cpu,__name__=x usage=1`,         // étiquette réservée
		`cpu,host=a usage=1 pasunnombre`, // horodatage invalide
		"",
	}, "\r\n")
	series, ignorees, err := DecoderInflux(strings.NewReader(corps), false, "", LimitesDecodageDefaut(), maintenant)
	if err != nil {
		t.Fatal(err)
	}
	if ignorees != 5 {
		t.Errorf("ignorées : %d", ignorees)
	}
	got := map[string]stockage.Point{}
	for _, s := range series {
		if len(s.Points) != 1 {
			t.Errorf("%s : %d points", cleSerie(s), len(s.Points))
		}
		got[cleSerie(s)] = s.Points[0]
	}
	ts := int64(1763114400000)
	for cle, att := range map[string]stockage.Point{
		"__name__=cpu_usage_idle,cpu=cpu-total,host=opnsense":      {T: ts, V: 97.5},
		"__name__=cpu_usage_user,cpu=cpu-total,host=opnsense":      {T: ts, V: 1.25},
		"__name__=mem_used_percent,host=opnsense":                  {T: maintenant.UnixMilli(), V: 42},
		"__name__=mem_available,host=opnsense":                     {T: maintenant.UnixMilli(), V: 1024},
		"__name__=pf_entries,host=opnsense":                        {T: maintenant.UnixMilli(), V: 1234},
		"__name__=pf_running,host=opnsense":                        {T: maintenant.UnixMilli(), V: 1},
		"__name__=net_bytes_recv,host=opnsense,interface=igb0 lan": {T: ts, V: 10},
		"__name__=ma_mesure_champ_x,ta_g=v=1":                      {T: ts, V: 1},
	} {
		if p, ok := got[cle]; !ok || p != att {
			t.Errorf("%s : %+v (présent %v), attendu %+v", cle, p, ok, att)
		}
	}
	if len(series) != 8 {
		t.Errorf("%d séries : %v", len(series), got)
	}

	// Précision et gzip.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("disk,host=o used_percent=50 1763114400\n"))
	_ = zw.Close()
	series, _, err = DecoderInflux(&gz, true, "s", LimitesDecodageDefaut(), maintenant)
	if err != nil || len(series) != 1 || series[0].Points[0].T != ts {
		t.Errorf("gzip + précision s : %+v %v", series, err)
	}
	if _, _, err := DecoderInflux(strings.NewReader("a b=1"), false, "h", LimitesDecodageDefaut(), maintenant); err == nil {
		t.Error("précision inconnue acceptée")
	}
	// Rien de valable : refusé.
	if _, _, err := DecoderInflux(strings.NewReader("n'importe quoi\nencore"), false, "", LimitesDecodageDefaut(), maintenant); err == nil {
		t.Error("corps sans aucune ligne valable accepté")
	}
	// Limites globales : taille, séries.
	lim := LimitesDecodageDefaut()
	lim.MaxDecompresse = 50
	if _, _, err := DecoderInflux(strings.NewReader(strings.Repeat("a b=1\n", 20)), false, "", lim, maintenant); err == nil {
		t.Error("corps trop gros accepté")
	}
	lim = LimitesDecodageDefaut()
	lim.MaxSeries = 2
	if _, _, err := DecoderInflux(strings.NewReader("a b=1,c=2,d=3"), false, "", lim, maintenant); err == nil {
		t.Error("trop de séries accepté")
	}
}

// Entrée HTTP : token « Token … » (Telegraf), nom de la machine imposé,
// /health sans token.
func TestEntreeInflux(t *testing.T) {
	b := nouveauBanc(t, nil)
	corps := []byte("cpu,host=opnsense,instance=autre usage_idle=90 \n")
	r := httptest.NewRequest(http.MethodPost, protocole.CheminInflux+"/api/v2/write?org=x&bucket=y&precision=ns", bytes.NewReader(corps))
	r.RemoteAddr = "192.0.2.1:4000"
	r.Header.Set("Authorization", "Token "+tokenPC)
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("écriture : %d %s", w.Code, w.Body)
	}
	trouve := false
	b.mem.Lire(func(series []stockage.Serie, _ []stockage.Ligne) {
		for _, s := range series {
			if s.Valeur("__name__") == "cpu_usage_idle" {
				trouve = true
				if s.Valeur("instance") != "pc" || s.Valeur("host") != "opnsense" {
					t.Errorf("étiquettes : %v", s.Labels)
				}
			}
		}
	})
	if !trouve {
		t.Error("série non écrite")
	}
	// Sans token : refusé.
	if c := b.envoyer(protocole.CheminInflux+"/api/v2/write", "", "", corps).Code; c != http.StatusUnauthorized {
		t.Errorf("sans token : %d", c)
	}
	// Ligne invalide seule : 400.
	if c := b.envoyer(protocole.CheminInflux+"/api/v2/write", tokenPC, "", []byte("rien")).Code; c != http.StatusBadRequest {
		t.Errorf("corps invalide : %d", c)
	}
	// Santé, sans token.
	r = httptest.NewRequest(http.MethodGet, protocole.CheminInflux+"/health", nil)
	r.RemoteAddr = "192.0.2.1:4000"
	w = httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"pass"`) {
		t.Errorf("health : %d %s", w.Code, w.Body)
	}
}

func FuzzDecoderInflux(f *testing.F) {
	for _, s := range []string{
		`cpu,cpu=cpu-total,host=o usage_idle=97.5,usage_user=1i 1763114400000000000`,
		`m\ x,t\,a=b\=c f="a \" b",g=t`,
		"a b=1\nc d=2u\n# x\n",
		`a,b= c=1`,
		`a c="non fermé`,
	} {
		f.Add([]byte(s))
	}
	maintenant := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, b []byte) {
		series, _, err := DecoderInflux(bytes.NewReader(b), false, "", LimitesDecodageDefaut(), maintenant)
		if err != nil {
			return
		}
		for _, s := range series {
			if !labelsValides(&s) {
				t.Fatalf("série invalide produite : %v", s.Labels)
			}
		}
	})
}
