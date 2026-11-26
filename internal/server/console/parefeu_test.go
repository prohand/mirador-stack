// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/fluxbloques"
	"mirador-stack/internal/server/stockage"
)

// Bloc « Pare-feu » (SPEC § 136) : réglage enregistré (action sensible),
// appliqué tout de suite aux lignes reçues, relu au redémarrage ; refus des
// réglages incohérents.
func TestReglagePareFeu(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	b.machinesApprouvees(t, "dns2")
	e, _ := fluxbloques.Nouveau(ctx, fluxbloques.Options{Maintenant: b.h.maintenant})
	so, _ := fluxbloques.Nouveau(ctx, fluxbloques.Options{Maintenant: b.h.maintenant})
	b.c.opt.Flux = fluxbloques.NouveauFlux(e, so)

	cl, secret := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/dns2")
	if !strings.Contains(corps, `id="parefeu"`) || !strings.Contains(corps, `value="parefeu"`) {
		t.Fatal("bloc Pare-feu absent de la page machine")
	}
	csrf := b.csrf(cl, "/machines/dns2")
	poster := func(v url.Values) {
		v.Set("csrf", csrf)
		v.Set("action", "parefeu")
		v.Set("code", b.code(secret))
		b.post(cl, "/machines/dns2", v)
		b.h.avancer(31 * time.Second) // code 2FA suivant
	}
	// Préfixes sans préfixe : refusé.
	poster(url.Values{"lecture": {"prefixes"}, "entrants": {"1"}})
	if r := b.c.reglagePareFeu("dns2"); r.Lecture != fluxbloques.LectureAuto {
		t.Fatalf("préfixes vides acceptés : %+v", r)
	}
	poster(url.Values{"lecture": {"prefixes"}, "prefixes": {"DROP_IN:, DROP_OUT:"}, "entrants": {"1"}, "sortants": {"1"}})
	r := b.c.reglagePareFeu("dns2")
	if r.Lecture != fluxbloques.LecturePrefixes || len(r.Prefixes) != 2 || !r.Sortants || r.GarderLignes {
		t.Fatalf("réglage : %+v", r)
	}
	// Appliqué tout de suite à la réception.
	garde := b.c.opt.Flux.Lignes(false, []stockage.Ligne{
		{T: b.h.maintenant().UnixMilli(), Msg: "DROP_OUT: IN= OUT=eth0 SRC=10.0.0.5 DST=8.8.4.4 PROTO=UDP DPT=53", Champs: map[string]string{"host": "dns2"}},
		{T: b.h.maintenant().UnixMilli(), Msg: "AUTRE: IN=eth0 OUT= SRC=45.0.0.2 PROTO=TCP DPT=22", Champs: map[string]string{"host": "dns2"}},
	})
	if so.Recus() != 1 || e.Recus() != 0 || len(garde) != 1 {
		t.Errorf("appliqué : sortants %d, entrants %d, gardées %d", so.Recus(), e.Recus(), len(garde))
	}
	// Relu en base (redémarrage), audit noté.
	b.c.opt.Flux.DefinirReglages(nil)
	b.c.chargerPareFeu(ctx)
	if r := b.c.reglagePareFeu("dns2"); r.Lecture != fluxbloques.LecturePrefixes {
		t.Errorf("relu : %+v", r)
	}
	var n int
	_ = b.db.QueryRow(`SELECT count(*) FROM audit WHERE action = 'parefeu dns2'`).Scan(&n)
	if n != 1 {
		t.Errorf("audit : %d", n)
	}
	// Retour au défaut : la clé disparaît.
	poster(url.Values{"lecture": {"auto"}, "entrants": {"1"}})
	if tous := lireReglagesPareFeu(ctx, b.db); len(tous) != 0 {
		t.Errorf("défaut gardé : %+v", tous)
	}
	// Page Flux bloqués : tableau des sources et choix du sens.
	_, corps, _ = b.get(cl, "/flux-bloques?sens=sortant")
	if !strings.Contains(corps, "Vers où vont les flux bloqués") || !strings.Contains(corps, `href="/flux-bloques?sens=sortant"`) ||
		!strings.Contains(corps, "Adresses locales qui essaient de sortir") {
		t.Error("page des sortants incomplète")
	}
}
