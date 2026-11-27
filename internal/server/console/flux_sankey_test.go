// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"testing"

	"mirador-stack/internal/server/fluxbloques"
)

// Six pare-feu, beaucoup de ports (cas du 27/11) : tous les pare-feu sont
// affichés et chaque lien tient dans les blocs de ses deux bouts.
func TestSankeyTousLesPareFeuEtLiensDansLesBlocs(t *testing.T) {
	for _, pfs := range [][]string{{"dns2", "proxy", "fw", "dns1", "web", "nas"}, {"fw", "dns1"}} {
		verifierSankey(t, pfs)
	}
}

func verifierSankey(t *testing.T, pfs []string) {
	t.Helper()
	pays := []string{"US", "CH", "FR", "DE", "GB", "BG", "NL"}
	b := fluxbloques.Bilan{PaysPareFeu: map[fluxbloques.Paire]int{}, PareFeuPort: map[fluxbloques.Paire]int{}}
	nPF, nPays, nPort := map[string]int{}, map[string]int{}, map[string]int{}
	for i, pf := range pfs {
		for j, p := range pays {
			n := 1 + (i*7+j*3)%9
			b.PaysPareFeu[fluxbloques.Paire{A: p, B: pf}] += n
			nPays[p] += n
			nPF[pf] += n
		}
		reste := nPF[pf]
		for k := 0; reste > 0; k++ {
			port := "udp/" + strconv.Itoa(1000+(i+k)%12)
			n := min(reste, 1+k%4)
			b.PareFeuPort[fluxbloques.Paire{A: pf, B: port}] += n
			nPort[port] += n
			reste -= n
		}
	}
	trier := func(m map[string]int) []fluxbloques.Compte {
		var out []fluxbloques.Compte
		for k, n := range m {
			out = append(out, fluxbloques.Compte{Nom: k, N: n})
		}
		for i := range out {
			for j := i + 1; j < len(out); j++ {
				if out[j].N > out[i].N || (out[j].N == out[i].N && out[j].Nom < out[i].Nom) {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return out
	}
	b.Pays, b.PareFeux, b.Ports = trier(nPays), trier(nPF), trier(nPort)
	for _, c := range b.Pays {
		b.Total += c.N
	}
	b.NbPays = len(b.Pays)
	s := construireSankey(b, url.Values{}, nil, nil)
	if s == nil || len(s.PareFeux) != len(pfs) {
		t.Fatalf("pare-feu affichés : %d, attendu %d", len(s.PareFeux), len(pfs))
	}
	for _, n := range s.PareFeux {
		if n.Cle == fluxbloques.Autres {
			t.Fatal("« autres » ne doit pas apparaître avec si peu de pare-feu")
		}
	}
	noeuds := append(append(append([]noeudSankey{}, s.Pays...), s.PareFeux...), s.Ports...)
	chiffre := regexp.MustCompile(`-?[0-9.]+`)
	for _, r := range s.Rubans {
		v := chiffre.FindAllString(r.D, -1)
		f := func(i int) float64 { x, _ := strconv.ParseFloat(v[i], 64); return x }
		x1, y1, x2, y2, y2b, y1b := f(0), f(1), f(6), f(7), f(9), f(15)
		for _, n := range noeuds {
			bord := n.X
			if n.Cle == r.A && bord+sankeyLargeur == x1 && (y1 < n.Y-0.2 || y1b > n.Y+n.H+0.2) {
				t.Errorf("lien %s → %s déborde au départ : %.1f-%.1f hors de %.1f-%.1f", r.A, r.B, y1, y1b, n.Y, n.Y+n.H)
			}
			if n.Cle == r.B && bord == x2 && (y2 < n.Y-0.2 || y2b > n.Y+n.H+0.2) {
				t.Errorf("lien %s → %s déborde à l'arrivée : %.1f-%.1f hors de %.1f-%.1f", r.A, r.B, y2, y2b, n.Y, n.Y+n.H)
			}
		}
	}
}

// Classements (SPEC § 139) : 6 lignes, 20 de plus repliées, « + N autres »
// et le reste des blocages ; protocole et numéro d'un port.
func TestClassementFlux(t *testing.T) {
	b := fluxbloques.Bilan{NbPays: 30, NbIP: 0}
	for i := 0; i < 30; i++ {
		b.Pays = append(b.Pays, fluxbloques.Compte{Nom: string(rune('A'+i%26)) + string(rune('A'+i/26)), N: 100 - i})
		b.Total += 100 - i
	}
	cols := (&Console{}).classementFlux(b, url.Values{}, nil, nil, false, false)
	p := cols[0]
	if len(p.Lignes) != 6 || len(p.Plus) != 20 || p.Autres != 24 || p.Lignes[0].Rang != 1 || p.Plus[0].Rang != 7 {
		t.Fatalf("pays : %d lignes, %d de plus, %d autres", len(p.Lignes), len(p.Plus), p.Autres)
	}
	if montres := 100 + 99 + 98 + 97 + 96 + 95; p.Reste != b.Total-montres {
		t.Errorf("reste : %d, attendu %d", p.Reste, b.Total-montres)
	}
	if len(cols[3].Lignes) != 0 || cols[3].Autres != 0 {
		t.Error("adresses : rien à montrer")
	}
	for nom, attendu := range map[string][2]string{"tcp/22": {"TCP", "22"}, "udp/10001": {"UDP", "10001"}, "icmpv6": {"ICMP", "icmpv6"}, "?": {"", "?"}} {
		if p, n := protoPort(nom); p != attendu[0] || n != attendu[1] {
			t.Errorf("protoPort(%q) = %q %q", nom, p, n)
		}
	}
}

// Grand parc (SPEC § 140) : pare-feu regroupés par site, puis un site seul.
func TestRegrouperEtRestreindreBilan(t *testing.T) {
	b := fluxbloques.Bilan{Total: 100, PaysPareFeu: map[fluxbloques.Paire]int{
		{A: "CN", B: "par-1"}: 40, {A: "US", B: "par-2"}: 30, {A: "CN", B: "lyo-1"}: 20, {A: "US", B: "inconnu"}: 10},
		PareFeuPort: map[fluxbloques.Paire]int{{A: "par-1", B: "tcp/22"}: 40, {A: "par-2", B: "tcp/22"}: 30, {A: "lyo-1", B: "tcp/23"}: 20, {A: "inconnu", B: "tcp/22"}: 10},
		PareFeux:    []fluxbloques.Compte{{Nom: "par-1", N: 40}, {Nom: "par-2", N: 30}, {Nom: "lyo-1", N: 20}, {Nom: "inconnu", N: 10}},
		Chemins:     []fluxbloques.Chemin{{Pays: "CN", PareFeu: "par-1", Port: "tcp/22", N: 40}, {Pays: "CN", PareFeu: "lyo-1", Port: "tcp/23", N: 20}}}
	sites := map[string]string{"par-1": "Paris", "par-2": "Paris", "lyo-1": "Lyon"}
	groupeDe := func(pf string) string {
		if s := sites[pf]; s != "" {
			return s
		}
		return "Sans site"
	}
	g, membres := regrouperBilan(b, groupeDe)
	if len(g.PareFeux) != 3 || g.PareFeux[0].Nom != "Paris" || g.PareFeux[0].N != 70 || membres["Paris"] != 2 || membres["Sans site"] != 1 {
		t.Fatalf("regroupé : %+v %v", g.PareFeux, membres)
	}
	if g.PaysPareFeu[fluxbloques.Paire{A: "US", B: "Paris"}] != 30 || g.PareFeuPort[fluxbloques.Paire{A: "Paris", B: "tcp/22"}] != 70 {
		t.Errorf("liens regroupés : %v %v", g.PaysPareFeu, g.PareFeuPort)
	}
	if s := construireSankey(g, url.Values{}, nil, nil); s == nil || len(s.PareFeux) != 3 {
		t.Error("schéma par site")
	}
	r := restreindreBilan(b, func(pf string) bool { return groupeDe(pf) == "Paris" })
	if r.Total != 70 || len(r.PareFeux) != 2 || r.NbPays != 2 || len(r.Chemins) != 1 || !r.SansAdresses || r.Ports[0].N != 70 {
		t.Errorf("Paris seul : total %d, %d pare-feu, %d pays, %d chemins", r.Total, len(r.PareFeux), r.NbPays, len(r.Chemins))
	}
	// Liens : changer de regroupement oublie site, machine et « tous » ;
	// choisir un site oublie la machine.
	v := url.Values{"vue": {"reseau"}, "par": {"site"}, "site": {"Paris"}, "pf": {"par-1"}, "tous": {"1"}}
	if l := lienFluxAvec(v, "par", "groupe", false); l != "/flux-bloques?par=groupe&vue=reseau" {
		t.Errorf("par : %s", l)
	}
	if l := lienFluxAvec(v, "site", "Lyon", false); l != "/flux-bloques?par=site&site=Lyon&vue=reseau" {
		t.Errorf("site : %s", l)
	}
}

// Grand parc (SPEC § 141) : sens des seuils et extrême d'une série.
func TestSensCritique(t *testing.T) {
	un, sept := 1.0, 7.0
	if sensCritique([]Seuil{{Couleur: "ok"}, {Valeur: &un, Couleur: "attention"}, {Valeur: &sept, Couleur: "erreur"}}) != 1 {
		t.Error("occupation : haut = grave")
	}
	if sensCritique([]Seuil{{Couleur: "erreur"}, {Valeur: &un, Couleur: "attention"}, {Valeur: &sept, Couleur: "ok"}}) != -1 {
		t.Error("jours restants : bas = grave")
	}
	if sensCritique(nil) != 1 {
		t.Error("sans seuil : haut = grave")
	}
	if extremeSerie([]float64{3, 9, math.NaN(), 1}, 1) != 9 || extremeSerie([]float64{3, 9, 1}, -1) != 1 {
		t.Error("extrêmes")
	}
}

// 1 500 disques : le disque plein de « zz-999 » reste affiché.
func TestGarderCritiques(t *testing.T) {
	var bs []vueBarre
	for i := 0; i < 1500; i++ {
		bs = append(bs, vueBarre{Nom: fmt.Sprintf("m-%04d", i), Valeur: float64(i % 50)})
	}
	bs = append(bs, vueBarre{Nom: "zz-999", Valeur: 99})
	sort.Slice(bs, func(i, j int) bool { return bs[i].Nom < bs[j].Nom })
	out, note := garderCritiques(bs, nil)
	if len(out) != barresMax || out[0].Nom != "zz-999" || note == "" {
		t.Fatalf("%d barres, première %s, note %q", len(out), out[0].Nom, note)
	}
	if out, note := garderCritiques(bs[:10], nil); len(out) != 10 || note != "" {
		t.Error("peu de barres : rien ne change")
	}
}
