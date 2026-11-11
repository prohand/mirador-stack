// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"strconv"
	"testing"
	"time"

	"mirador-stack/internal/server/stockage"
)

func TestLimiteur(t *testing.T) {
	l := NouveauLimiteur(1, 3, 100)
	t0 := reception
	for i := 0; i < 3; i++ {
		if !l.Prendre("a", 1, t0) {
			t.Fatalf("requête %d refusée dans la rafale", i)
		}
	}
	if l.Prendre("a", 1, t0) {
		t.Fatal("la 4e requête doit être refusée")
	}
	if !l.Prendre("b", 1, t0) {
		t.Fatal("une autre clé a son propre seau")
	}
	if !l.Prendre("a", 1, t0.Add(1100*time.Millisecond)) {
		t.Fatal("un jeton revient après 1 s")
	}
	// Lot plus gros que le reste : passe, mais la clé doit rembourser.
	l2 := NouveauLimiteur(1, 10, 100)
	if !l2.Prendre("m", 25, t0) || l2.Prendre("m", 1, t0.Add(5*time.Second)) {
		t.Fatal("dette non respectée")
	}
	if a := l2.Attente("m"); a < 10 {
		t.Errorf("attente conseillée %d s", a)
	}
	if !l2.Prendre("m", 1, t0.Add(16*time.Second)) {
		t.Fatal("dette remboursée après 16 s")
	}
}

func TestLimiteurBorne(t *testing.T) {
	l := NouveauLimiteur(1, 2, 10)
	for i := 0; i < 10; i++ {
		l.Prendre(strconv.Itoa(i), 1, reception)
	}
	if l.Prendre("nouvelle", 1, reception) {
		t.Fatal("table pleine de seaux actifs : nouvelle clé refusée")
	}
	// Plus tard, les seaux sont pleins à nouveau : ils sont recyclés.
	if !l.Prendre("nouvelle", 1, reception.Add(time.Minute)) {
		t.Fatal("les seaux pleins doivent être recyclés")
	}
	if len(l.seaux) > 10 {
		t.Fatalf("%d clés suivies", len(l.seaux))
	}
}

func serie(nom string, labels ...string) stockage.Serie {
	s := stockage.Serie{Labels: []stockage.Label{{Nom: "__name__", Valeur: nom}}, Points: []stockage.Point{{T: 1, V: 1}}}
	for i := 0; i+1 < len(labels); i += 2 {
		s.Remplacer(labels[i], labels[i+1])
	}
	return s
}

func TestQuotas(t *testing.T) {
	q := NouveauxQuotas(LimitesSeries{ParMachine: 5, ParNom: 3, Total: 8, Inactivite: time.Hour})
	var lot []stockage.Serie
	for i := 0; i < 5; i++ {
		lot = append(lot, serie("explose", "id", strconv.Itoa(i)))
	}
	lot = append(lot, serie("normal"))
	gardees, refusees := q.Filtrer(1, lot, reception)
	if len(gardees) != 4 || refusees != 2 {
		t.Fatalf("cardinalité : gardées %d, refusées %d", len(gardees), refusees)
	}
	// Les séries déjà connues passent toujours.
	gardees, refusees = q.Filtrer(1, []stockage.Serie{serie("explose", "id", "0"), serie("normal")}, reception)
	if len(gardees) != 2 || refusees != 0 {
		t.Fatalf("séries connues : gardées %d, refusées %d", len(gardees), refusees)
	}
	// Quota par machine : 4 séries + 1 = 5, puis refus.
	gardees, refusees = q.Filtrer(1, []stockage.Serie{serie("b"), serie("c")}, reception)
	if len(gardees) != 1 || refusees != 1 || q.Actives(1) != 5 {
		t.Fatalf("quota machine : gardées %d, refusées %d, actives %d", len(gardees), refusees, q.Actives(1))
	}
	// Plafond total (8) : la machine 2 n'en obtient que 3.
	gardees, _ = q.Filtrer(2, []stockage.Serie{serie("a"), serie("b"), serie("c"), serie("d")}, reception)
	if len(gardees) != 3 {
		t.Fatalf("plafond total : %d", len(gardees))
	}
	// Après une heure de silence, les séries sont oubliées.
	q.Nettoyer(reception.Add(2 * time.Hour))
	if q.Actives(1) != 0 || q.total != 0 || len(q.machines) != 0 {
		t.Fatalf("nettoyage : actives %d, total %d", q.Actives(1), q.total)
	}
}

func TestJugerHorloge(t *testing.T) {
	h := LimitesHorlogeDefaut()
	cas := []struct {
		ecart time.Duration
		dec   Decision
	}{
		{0, HeureBonne}, {20 * time.Second, HeureBonne}, {-29 * time.Second, HeureBonne},
		{2 * time.Minute, HeureCorrige}, {-9 * time.Minute, HeureCorrige},
		{11 * time.Minute, HeureRefuse}, {-time.Hour, HeureRefuse},
	}
	for _, c := range cas {
		dec, d := h.Juger(reception.Add(c.ecart), reception)
		if dec != c.dec || d != c.ecart {
			t.Errorf("écart %v : décision %d, décalage %v", c.ecart, dec, d)
		}
	}
}
