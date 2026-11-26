// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/fluxbloques"
)

// Page « Flux bloqués » (SPEC § 135, maquette validée le 26/11) : carte du
// monde et « Mon réseau » (pays → pare-feu → ports), fil en direct poussé
// par le serveur, classements. Tout est calculé sur le serveur à partir du
// magasin des flux bloqués (jamais en relisant les logs).

func (c *Console) routesFlux() {
	c.mux.HandleFunc("GET /flux-bloques", c.lecture(c.pageFlux))
	c.mux.HandleFunc("GET /flux-bloques/direct", c.lecture(c.fluxBloquesDirect))
}

// Périodes proposées.
var periodesFlux = []struct {
	Cle, Nom string
	Duree    time.Duration
	Barres   int
}{
	{"direct", "Direct", time.Hour, 60},
	{"1h", "1 h", time.Hour, 60},
	{"24h", "24 h", 24 * time.Hour, 24},
	{"7j", "7 j", 7 * 24 * time.Hour, 84},
	{"30j", "30 j", 30 * 24 * time.Hour, 120},
}

// nomsServices : nom courant des ports les plus visés (affichage seulement).
var nomsServices = map[string]string{
	"tcp/21": "ftp", "tcp/22": "ssh", "tcp/23": "telnet", "tcp/25": "smtp", "udp/53": "dns", "tcp/53": "dns",
	"tcp/80": "http", "udp/123": "ntp", "udp/161": "snmp", "tcp/443": "https", "udp/443": "quic", "tcp/445": "smb",
	"udp/500": "ipsec", "tcp/1433": "mssql", "udp/1194": "openvpn", "tcp/2375": "docker", "tcp/3306": "mysql",
	"tcp/3389": "rdp", "udp/3478": "stun", "udp/5060": "sip", "tcp/5060": "sip", "tcp/5432": "postgres",
	"tcp/5900": "vnc", "tcp/6379": "redis", "tcp/8080": "http", "tcp/8443": "https", "tcp/9200": "elasticsearch",
	"tcp/11211": "memcached", "tcp/27017": "mongodb", "udp/51820": "wireguard",
}

var (
	reFiltrePays = regexp.MustCompile(`^[A-Z]{2}$`)
	reFiltrePort = regexp.MustCompile(`^[a-z0-9]{1,12}(/[0-9]{1,5})?$`)
	reFiltrePF   = regexp.MustCompile(`^[\p{L}\p{N} ._:@-]{1,100}$`)
)

type lienFlux struct {
	Nom, Lien string
	Actif     bool
}

type rangFlux struct {
	Nom, Sous, Code, Lien string
	N, Pct                int
	PareFeux              int
}

type barreFlux struct {
	X, Y, L, H float64
	Pic        bool
	Titre      string
}

type ligneFil struct {
	ID                             int64
	Heure, Pays, IP, PareFeu, Port string
	Service, Regle                 string
	N                              int
}

type donneesFlux struct {
	Periode, Vue               string
	Periodes, Vues             []lienFlux
	Direct                     bool
	FiltreTexte, SansFiltre    string
	B                          fluxbloques.Bilan
	ParMinute                  int
	Spark                      string // points de la petite courbe (30 min)
	Comparaison                string
	ComparaisonClasse          string
	PortPrincipal              string
	ServicePrincipal           string
	Carte                      []paysVue
	CarteVide                  string
	Maison                     [2]float64
	Barres                     []barreFlux
	Axe                        [3]string
	PicTexte                   string
	Fil                        []ligneFil
	Pays, Ports, PareFeux, IPs []rangFlux
	Sankey                     *sankeyVue
	Note                       string
	GeoIP                      bool
	NomPeriode                 string
	Centres                    string // JSON : centre de chaque pays (points du direct)
	LienDirect                 string
	Sources                    []sourceFlux // état de la collecte OPNsense par collecteur
	Sens                       string       // entrant ou sortant (SPEC § 136)
	Sensz                      []lienFlux
	Sortant                    bool
	Locales                    []rangFlux // sortants : adresses locales qui essaient de sortir
	Table                      []ligneSource
	TableLues, TableSans       int
}

// lienFluxAvec : adresse de la page avec un paramètre changé (les autres gardés).
func lienFluxAvec(v url.Values, cle, val string, sansFiltre bool) string {
	w := url.Values{}
	for _, k := range []string{"sens", "periode", "vue", "pays", "pf", "port"} {
		if x := v.Get(k); x != "" && !(sansFiltre && (k == "pays" || k == "pf" || k == "port")) {
			w.Set(k, x)
		}
	}
	if cle != "" {
		for _, k := range []string{"pays", "pf", "port"} {
			if cle == k {
				w.Del("pays")
				w.Del("pf")
				w.Del("port")
			}
		}
		if val == "" {
			w.Del(cle)
		} else {
			w.Set(cle, val)
		}
	}
	if len(w) == 0 {
		return "/flux-bloques"
	}
	return "/flux-bloques?" + w.Encode()
}

func (c *Console) pageFlux(q *req) {
	v := q.r.URL.Query()
	d := donneesFlux{Periode: v.Get("periode"), Vue: v.Get("vue"), Sens: protocole.Entrant}
	if d.Vue != "reseau" {
		d.Vue = "monde"
	}
	if v.Get("sens") == protocole.Sortant {
		d.Sens, d.Sortant = protocole.Sortant, true
	}
	d.Sensz = []lienFlux{{"Entrants", lienFluxAvec(v, "sens", "", true), !d.Sortant},
		{"Sortants", lienFluxAvec(v, "sens", protocole.Sortant, true), d.Sortant}}
	p := periodesFlux[0]
	for _, x := range periodesFlux {
		if x.Cle == d.Periode {
			p = x
		}
	}
	d.Periode, d.Direct, d.NomPeriode = p.Cle, p.Cle == "direct", p.Nom
	if d.Direct {
		d.NomPeriode = "1 h"
	}
	d.LienDirect = lienFluxAvec(v, "periode", "direct", false)
	for _, x := range periodesFlux {
		d.Periodes = append(d.Periodes, lienFlux{x.Nom, lienFluxAvec(v, "periode", x.Cle, false), x.Cle == p.Cle})
	}
	d.Vues = []lienFlux{{"🌍 Monde", lienFluxAvec(v, "vue", "monde", false), d.Vue == "monde"},
		{"🔀 Mon réseau", lienFluxAvec(v, "vue", "reseau", false), d.Vue == "reseau"}}
	var f fluxbloques.Filtre
	switch {
	case reFiltrePays.MatchString(v.Get("pays")):
		f.Pays = v.Get("pays")
		d.FiltreTexte = drapeau(f.Pays) + " " + nomPaysOuCode(f.Pays)
	case reFiltrePF.MatchString(v.Get("pf")):
		f.PareFeu = v.Get("pf")
		d.FiltreTexte = "pare-feu " + f.PareFeu
	case reFiltrePort.MatchString(v.Get("port")):
		f.Port = v.Get("port")
		d.FiltreTexte = "port " + f.Port
	}
	if !f.Vide() {
		d.SansFiltre = lienFluxAvec(v, "", "", true)
	}
	if c.opt.Flux == nil {
		c.rendre(q, "flux", "Flux bloqués", "tableaux", d)
		return
	}
	fin := q.t
	debut := fin.Add(-p.Duree)
	if p.Duree <= time.Hour {
		debut = fin.Truncate(time.Minute).Add(-59 * time.Minute)
	}
	mag := c.opt.Flux.Sens(d.Sens)
	b, err := mag.Bilan(q.r.Context(), debut, fin, f, p.Barres)
	if err != nil {
		c.flash(q, "", "lecture des flux bloqués : "+err.Error())
	}
	d.B = b
	minutes, _ := mag.ParMinute(30)
	if len(minutes) > 0 {
		d.ParMinute = minutes[len(minutes)-1]
		d.Spark = sparkFlux(minutes)
	}
	if b.AvantConnu && b.Avant > 0 {
		pct := int(math.Round(float64(b.Total-b.Avant) * 100 / float64(b.Avant)))
		d.Comparaison = fmt.Sprintf("%+d %% / période d'avant", pct)
		if p.Duree == 24*time.Hour {
			d.Comparaison = fmt.Sprintf("%+d %% / hier", pct)
		}
		d.Comparaison = strings.Replace(d.Comparaison, "-", "−", 1)
		d.ComparaisonClasse = "moins"
		if pct > 0 {
			d.ComparaisonClasse = "plus"
		}
	}
	if len(b.Ports) > 0 && b.Ports[0].Nom != fluxbloques.Autres {
		d.PortPrincipal, d.ServicePrincipal = b.Ports[0].Nom, nomsServices[b.Ports[0].Nom]
	}
	// Carte : pays colorés, point d'arrivée « vos pare-feu ».
	d.GeoIP = c.opt.GeoIP != nil && lireInterface(q.r.Context(), c.db).GeoIP
	parPays := map[string]int{}
	for _, x := range b.Pays {
		parPays[x.Nom] = x.N
	}
	d.Carte = carteFlux(parPays)
	d.Centres = centresJSON
	if centre, ok := centresPays[lireInterface(q.r.Context(), c.db).PaysCarte]; ok {
		d.Maison = centre
	} else {
		d.Maison = centresPays["FR"]
	}
	switch {
	case !d.GeoIP:
		d.CarteVide = "GéoIP désactivée (Réglages › Interface) : pays inconnus"
	case b.Total == 0:
		d.CarteVide = "aucun flux bloqué sur la période"
	case b.NbPays == 0:
		if fichier, _, _ := c.opt.GeoIP.Etat(); fichier.IsZero() {
			d.CarteVide = "base GéoIP pas encore téléchargée (Réglages › Interface)"
		}
	}
	d.Barres, d.Axe, d.PicTexte = barresFlux(b)
	// Fil des derniers blocages (le direct ajoute les suivants).
	ev, _ := mag.Depuis(0, 12)
	for i := len(ev) - 1; i >= 0; i-- {
		d.Fil = append(d.Fil, ligneFilDe(ev[i]))
	}
	// Classements.
	d.Pays = rangsFlux(b.Pays, 6, func(r *rangFlux) {
		r.Code, r.Nom = r.Nom, nomPaysOuCode(r.Nom)
		if r.Code != "" && r.Code != fluxbloques.Autres {
			r.Lien = lienFluxAvec(v, "pays", r.Code, false)
		}
	})
	d.Ports = rangsFlux(b.Ports, 6, func(r *rangFlux) {
		r.Sous = nomsServices[r.Nom]
		if r.Nom != fluxbloques.Autres && r.Nom != "?" {
			r.Lien = lienFluxAvec(v, "port", r.Nom, false)
		}
	})
	d.PareFeux = rangsFlux(b.PareFeux, 6, func(r *rangFlux) {
		if r.Nom != fluxbloques.Autres {
			r.Lien = lienFluxAvec(v, "pf", r.Nom, false)
		}
	})
	d.IPs = rangsFlux(b.IPs, 6, func(r *rangFlux) {
		if c.opt.GeoIP != nil && d.GeoIP {
			r.Code = paysDeTexte(c, r.Nom)
		}
		r.Lien = "/logs?mots=" + url.QueryEscape(r.Nom) + "&periode=24h"
	})
	for _, x := range b.IPs {
		if x.PareFeux >= 2 && d.Note == "" {
			d.Note = fmt.Sprintf("%s a été bloquée par %d pare-feu sur la période : la même adresse essaie plusieurs de vos machines.", x.Nom, x.PareFeux)
		}
	}
	d.Sankey = construireSankey(b, v)
	d.Locales = rangsFlux(b.Sources, 6, func(*rangFlux) {})
	d.Sources = nil
	for _, x := range c.sourcesFlux(q.r.Context(), q.t) {
		if x.Classe != "ok" { // l'encadré ne reste que pour signaler un réglage à faire
			d.Sources = append(d.Sources, x)
		}
	}
	d.Table = c.tableauSources(q.r.Context(), q.t)
	for _, l := range d.Table {
		if l.Bloques > 0 {
			d.TableLues++
		}
		if l.Classe == "erreur" {
			d.TableSans++
		}
	}
	c.rendre(q, "flux", "Flux bloqués", "tableaux", d)
}

// sourceFlux : où en est le comptage filterlog d'un collecteur (retour du
// 26/11 : « fw n'apparaît pas » ; la page dit où ça coince).
type sourceFlux struct {
	Collecteur, Texte, Classe string
}

// sourcesFlux : un état par collecteur qui a une écoute syslog.
func (c *Console) sourcesFlux(ctx context.Context, t time.Time) []sourceFlux {
	ecoute := c.parEtiquette(ctx, `max by (instance) (mirador_collecteur_syslog_ecoute)`, t, "instance")
	if len(ecoute) == 0 {
		return nil
	}
	compteur := c.parEtiquette(ctx, `max by (instance) (mirador_collecteur_flux_bloques_total)`, t, "instance")
	blocages := c.parEtiquette(ctx, `sum by (instance) (increase(mirador_collecteur_flux_bloques_total[1h]))`, t, "instance")
	filterlog := c.parEtiquette(ctx, `sum by (instance) (increase(mirador_collecteur_syslog_refus_total{motif="filterlog"}[1h]))`, t, "instance")
	var out []sourceFlux
	for inst, e := range ecoute {
		s := sourceFlux{Collecteur: inst}
		_, aJour := compteur[inst]
		switch {
		case e < 1:
			s.Texte, s.Classe = "écoute syslog fermée : rien ne peut arriver d'OPNsense (page du collecteur)", "erreur"
		case !aJour:
			s.Texte, s.Classe = "collecteur à mettre à jour (0.6.27 ou plus) : il ne compte pas encore les blocages d'OPNsense", "attention"
		case blocages[inst] >= 1:
			s.Texte, s.Classe = fmt.Sprintf("%s blocages entrants comptés sur la dernière heure", nombreLisible(int(blocages[inst]))), "ok"
		case filterlog[inst] >= 1:
			s.Texte, s.Classe = fmt.Sprintf("%s lignes filterlog reçues sur 1 h, mais aucun blocage entrant venu d'Internet : "+
				"cocher la journalisation des règles de blocage du WAN (ou des paquets bloqués par la règle par défaut)", nombreLisible(int(filterlog[inst]))), "attention"
		default:
			s.Texte, s.Classe = "aucune ligne filterlog reçue depuis 1 h : dans OPNsense, Système › Paramètres › Journalisation › Distant, "+
				"destination du collecteur : ajouter l'application filterlog (« filter ») et le niveau « info » (les blocages sont au niveau info), "+
				"puis Apply (si « Garder filterlog » est coché, les lignes vont dans les logs : vérifier là)", "attention"
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Collecteur < out[j].Collecteur })
	return out
}

func nomPaysOuCode(code string) string {
	switch code {
	case "":
		return "Pays inconnu"
	case fluxbloques.Autres:
		return "Autres"
	}
	if n, ok := nomsPays[code]; ok {
		return n
	}
	return code
}

// nomCourtPays : nom du pays pour la colonne de gauche de « Mon réseau »
// (18 caractères au plus ; quelques noms officiels raccourcis).
func nomCourtPays(code string) string {
	if n, ok := map[string]string{"CN": "Chine", "KR": "Corée du Sud", "KP": "Corée du Nord", "GB": "Royaume-Uni",
		"CD": "RD Congo", "DO": "Rép. dominicaine", "CF": "Centrafrique", "AE": "Émirats arabes unis"}[code]; ok {
		return n
	}
	n := []rune(nomPaysOuCode(code))
	if len(n) > 18 {
		return string(n[:17]) + "…"
	}
	return string(n)
}

func paysDeTexte(c *Console, ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	return c.opt.GeoIP.Pays(a)
}

func rangsFlux(cs []fluxbloques.Compte, n int, f func(*rangFlux)) []rangFlux {
	if len(cs) == 0 {
		return nil
	}
	plusGros := max(cs[0].N, 1)
	var out []rangFlux
	for i, x := range cs {
		if i >= n {
			break
		}
		r := rangFlux{Nom: x.Nom, N: x.N, Pct: min(100, x.N*100/plusGros), PareFeux: x.PareFeux}
		f(&r)
		out = append(out, r)
	}
	return out
}

func ligneFilDe(e fluxbloques.Evenement) ligneFil {
	return ligneFil{ID: e.ID, Heure: time.UnixMilli(e.T).In(fuseau.Load()).Format("15:04:05"), Pays: e.Pays, IP: e.IP,
		PareFeu: e.PareFeu, Port: e.Port, Service: nomsServices[e.Port], Regle: e.Regle, N: e.N}
}

// sparkFlux : petite courbe (100 × 24) des dernières minutes.
func sparkFlux(vals []int) string {
	m := 1
	for _, v := range vals {
		m = max(m, v)
	}
	var b strings.Builder
	for i, v := range vals {
		x := float64(i) * 100 / float64(max(len(vals)-1, 1))
		y := 22 - float64(v)*20/float64(m)
		fmt.Fprintf(&b, "%.1f,%.1f ", x, y)
	}
	return strings.TrimSpace(b.String())
}

// barresFlux : frise (600 × 70), axe (début, milieu, fin) et texte du pic.
func barresFlux(b fluxbloques.Bilan) ([]barreFlux, [3]string, string) {
	var axe [3]string
	if len(b.Barres) == 0 {
		return nil, axe, ""
	}
	m := 1
	for _, x := range b.Barres {
		m = max(m, x.N)
	}
	format := "15:04"
	if b.LargeurBarre >= time.Hour {
		format = "02/01 15h"
	}
	loc := fuseau.Load()
	l := 600 / float64(len(b.Barres))
	out := make([]barreFlux, 0, len(b.Barres))
	for i, x := range b.Barres {
		h := float64(x.N) * 62 / float64(m)
		if x.N > 0 {
			h = max(h, 2)
		}
		out = append(out, barreFlux{X: float64(i)*l + l*0.1, L: l * 0.8, Y: 66 - h, H: h, Pic: i == b.Pic,
			Titre: x.Debut.In(loc).Format(format) + " : " + strconv.Itoa(x.N) + " bloqué(s)"})
	}
	axe = [3]string{b.Barres[0].Debut.In(loc).Format(format), b.Barres[len(b.Barres)/2].Debut.In(loc).Format(format),
		b.Barres[len(b.Barres)-1].Debut.In(loc).Format(format)}
	pic := ""
	if b.Pic >= 0 {
		x := b.Barres[b.Pic]
		pic = "pic à " + x.Debut.In(loc).Format(format) + " : " + strconv.Itoa(x.N)
		if b.PicIP != "" {
			pic += ", surtout " + b.PicIP
		}
		if b.PicPort != "" {
			pic += " sur " + b.PicPort
		}
	}
	return out, axe, pic
}

// --- Direct ---------------------------------------------------------------------

const (
	maxFluxDirect   = 8
	dureeFluxDirect = 30 * time.Minute
)

var nbFluxDirect atomic.Int64

// evenementDirect : ce que reçoit la page (texte seulement ; le navigateur
// l'affiche avec textContent).
type evenementDirect struct {
	ID      int64  `json:"id"`
	Heure   string `json:"heure"`
	Pays    string `json:"pays"`
	Drapeau string `json:"drapeau"`
	IP      string `json:"ip"`
	PareFeu string `json:"pf"`
	Port    string `json:"port"`
	Service string `json:"service"`
	N       int    `json:"n"`
}

// fluxBloquesDirect : blocages poussés à la page (text/event-stream), regroupés
// au plus une fois par seconde ; « minute » : blocages de la dernière minute.
func (c *Console) fluxBloquesDirect(q *req) {
	if c.opt.Flux == nil {
		http.Error(q.w, "indisponible", http.StatusNotFound)
		return
	}
	if nbFluxDirect.Add(1) > maxFluxDirect {
		nbFluxDirect.Add(-1)
		http.Error(q.w, "trop de pages en direct", http.StatusTooManyRequests)
		return
	}
	defer nbFluxDirect.Add(-1)
	mag := c.opt.Flux.Entrants
	if q.r.URL.Query().Get("sens") == protocole.Sortant {
		mag = c.opt.Flux.Sortants
	}
	depuis, _ := strconv.ParseInt(q.r.URL.Query().Get("depuis"), 10, 64)
	if id, err := strconv.ParseInt(q.r.Header.Get("Last-Event-ID"), 10, 64); err == nil && id > depuis {
		depuis = id
	}
	if depuis <= 0 {
		depuis = mag.DernierID()
	}
	h := q.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(q.w)
	ecrire := func(s string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(delaiEcritFlux))
		if _, err := io.WriteString(q.w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	q.w.WriteHeader(http.StatusOK)
	if !ecrire(": \n\n") {
		return
	}
	fin := time.NewTimer(dureeFluxDirect)
	defer fin.Stop()
	minute := time.NewTicker(10 * time.Second)
	defer minute.Stop()
	for {
		ev, signal := mag.Depuis(depuis, 50)
		if len(ev) > 0 {
			depuis = ev[len(ev)-1].ID
			out := make([]evenementDirect, 0, len(ev))
			for _, e := range ev {
				l := ligneFilDe(e)
				out = append(out, evenementDirect{ID: e.ID, Heure: l.Heure, Pays: e.Pays, Drapeau: drapeau(e.Pays), IP: e.IP,
					PareFeu: e.PareFeu, Port: e.Port, Service: l.Service, N: e.N})
			}
			b, _ := json.Marshal(out)
			if !ecrire("id: " + strconv.FormatInt(depuis, 10) + "\nevent: flux\ndata: " + string(b) + "\n\n") {
				return
			}
		}
		select {
		case <-q.r.Context().Done():
			return
		case <-fin.C:
			ecrire("event: fin\ndata: {}\n\n")
			return
		case <-minute.C:
			mins, _ := mag.ParMinute(30)
			b, _ := json.Marshal(map[string]any{"minute": mins[len(mins)-1], "spark": sparkFlux(mins)})
			if !ecrire("event: minute\ndata: " + string(b) + "\n\n") {
				return
			}
		case <-signal:
			// Regroupement : une rafale de blocages = un seul envoi par seconde.
			select {
			case <-q.r.Context().Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// --- « Mon réseau » : trois colonnes reliées -------------------------------------

type noeudSankey struct {
	Nom, Sous, Cle, Lien string
	N                    int
	X, Y, H              float64
	TY                   float64 // position du texte
}

type rubanSankey struct {
	D, Centre, Classe, Titre string
	A, B                     string // clés des deux bouts (direct)
}

type sankeyVue struct {
	Pays, PareFeux, Ports []noeudSankey
	Rubans                []rubanSankey
	Hauteur               float64
	Estime                bool
}

const (
	sankeyHaut     = 400.0
	sankeyEcart    = 14.0
	sankeyMinNoeud = 30.0
	sankeyLargeur  = 14.0
)

// regrouperTop : les n premiers, le reste dans « autres ».
func regrouperTop(cs []fluxbloques.Compte, n int) (garder map[string]bool, ordre []string) {
	garder = map[string]bool{}
	for i, x := range cs {
		if i >= n || x.Nom == fluxbloques.Autres {
			break
		}
		garder[x.Nom] = true
		ordre = append(ordre, x.Nom)
	}
	return garder, ordre
}

func construireSankey(b fluxbloques.Bilan, v url.Values) *sankeyVue {
	if b.Total == 0 {
		return nil
	}
	gPays, oPays := regrouperTop(b.Pays, 4)
	gPF, oPF := regrouperTop(b.PareFeux, 4)
	gPort, oPort := regrouperTop(b.Ports, 5)
	cle := func(g map[string]bool, k string) string {
		if g[k] {
			return k
		}
		return fluxbloques.Autres
	}
	gauche, droite := map[fluxbloques.Paire]int{}, map[fluxbloques.Paire]int{}
	for k, n := range b.PaysPareFeu {
		gauche[fluxbloques.Paire{A: cle(gPays, k.A), B: cle(gPF, k.B)}] += n
	}
	for k, n := range b.PareFeuPort {
		droite[fluxbloques.Paire{A: cle(gPF, k.A), B: cle(gPort, k.B)}] += n
	}
	somme := func(m map[fluxbloques.Paire]int, aGauche bool) map[string]int {
		out := map[string]int{}
		for k, n := range m {
			if aGauche {
				out[k.A] += n
			} else {
				out[k.B] += n
			}
		}
		return out
	}
	nPays, nPFin, nPFout, nPort := somme(gauche, true), somme(gauche, false), somme(droite, true), somme(droite, false)
	avecAutres := func(ordre []string, m map[string]int) []string {
		if m[fluxbloques.Autres] > 0 {
			ordre = append(ordre, fluxbloques.Autres)
		}
		var out []string
		for _, k := range ordre {
			if m[k] > 0 {
				out = append(out, k)
			}
		}
		return out
	}
	nPF := map[string]int{}
	for k, n := range nPFin {
		nPF[k] = max(n, nPFout[k])
	}
	for k, n := range nPFout {
		nPF[k] = max(n, nPFin[k])
	}
	colPays, colPF, colPort := avecAutres(oPays, nPays), avecAutres(oPF, nPF), avecAutres(oPort, nPort)
	if len(colPays) == 0 || len(colPF) == 0 {
		return nil
	}
	s := &sankeyVue{Estime: b.Estime}
	total := 0
	for _, n := range nPF {
		total += n
	}
	placer := func(col []string, m map[string]int, x float64, nom func(string) (string, string, string)) []noeudSankey {
		k := (sankeyHaut - sankeyEcart*float64(len(col)-1)) / float64(max(total, 1))
		y := 24.0
		var out []noeudSankey
		for _, c := range col {
			h := float64(m[c]) * k
			nm, sous, lien := nom(c)
			out = append(out, noeudSankey{Nom: nm, Sous: sous, Cle: c, Lien: lien, N: m[c], X: x, Y: y, H: h, TY: y + max(h, 16)/2})
			y += max(h, sankeyMinNoeud) + sankeyEcart
		}
		s.Hauteur = max(s.Hauteur, y+30)
		return out
	}
	s.Pays = placer(colPays, nPays, 150, func(c string) (string, string, string) {
		if c == fluxbloques.Autres {
			return fmt.Sprintf("Autres pays (%d)", max(b.NbPays-len(gPays), 0)), "", ""
		}
		return strings.TrimSpace(drapeau(c) + " " + nomCourtPays(c)), "", lienFluxAvec(v, "pays", c, false)
	})
	s.PareFeux = placer(colPF, nPF, 470, func(c string) (string, string, string) {
		if c == fluxbloques.Autres {
			return fmt.Sprintf("%d autres", max(len(b.PareFeux)-len(gPF), 0)), "machines", ""
		}
		return c, "", lienFluxAvec(v, "pf", c, false)
	})
	s.Ports = placer(colPort, nPort, 790, func(c string) (string, string, string) {
		if c == fluxbloques.Autres {
			return "Autres ports", "", ""
		}
		return strings.TrimSpace(c + " " + nomsServices[c]), "", lienFluxAvec(v, "port", c, false)
	})
	k := (sankeyHaut - sankeyEcart*float64(len(colPF)-1)) / float64(max(total, 1))
	relier := func(src, dst []noeudSankey, m map[fluxbloques.Paire]int, classe string) {
		offS, offD := map[string]float64{}, map[string]float64{}
		for i, a := range src {
			for _, bn := range dst {
				n := m[fluxbloques.Paire{A: a.Cle, B: bn.Cle}]
				if n == 0 {
					continue
				}
				h := float64(n) * k
				x1, y1 := a.X+sankeyLargeur, a.Y+offS[a.Cle]
				x2, y2 := bn.X, bn.Y+offD[bn.Cle]
				mx := (x1 + x2) / 2
				s.Rubans = append(s.Rubans, rubanSankey{
					D: fmt.Sprintf("M%.1f %.1fC%.1f %.1f %.1f %.1f %.1f %.1fL%.1f %.1fC%.1f %.1f %.1f %.1f %.1f %.1fZ",
						x1, y1, mx, y1, mx, y2, x2, y2, x2, y2+h, mx, y2+h, mx, y1+h, x1, y1+h),
					Centre: fmt.Sprintf("M%.1f %.1fC%.1f %.1f %.1f %.1f %.1f %.1f", x1, y1+h/2, mx, y1+h/2, mx, y2+h/2, x2, y2+h/2),
					Classe: classe + strconv.Itoa(min(i, 4)), Titre: fmt.Sprintf("%s → %s : %d", a.Nom, bn.Nom, n),
					A: a.Cle, B: bn.Cle})
				offS[a.Cle] += h
				offD[bn.Cle] += h
			}
		}
	}
	relier(s.Pays, s.PareFeux, gauche, "g")
	relier(s.PareFeux, s.Ports, droite, "m")
	return s
}

var centresJSON = func() string {
	b, _ := json.Marshal(centresPays)
	return string(b)
}()

// --- Centres des pays (points et traits de la carte) -----------------------------

// centresPays : point représentatif de chaque pays sur la carte (médiane
// des points du contour ; corrigé à la main pour les pays dont les
// territoires lointains déplacent la médiane).
var centresPays = func() map[string][2]float64 {
	m := make(map[string][2]float64, len(paysCarte))
	for _, p := range paysCarte {
		var xs, ys []float64
		for i, champ := range strings.FieldsFunc(p.D, func(r rune) bool { return r == ' ' || r == 'M' || r == 'L' || r == 'Z' }) {
			f, err := strconv.ParseFloat(champ, 64)
			if err != nil {
				continue
			}
			if i%2 == 0 {
				xs = append(xs, f)
			} else {
				ys = append(ys, f)
			}
		}
		if len(xs) == 0 || len(ys) == 0 {
			continue
		}
		sort.Float64s(xs)
		sort.Float64s(ys)
		m[p.Code] = [2]float64{xs[len(xs)/2], ys[len(ys)/2]}
	}
	for k, v := range map[string][2]float64{"FR": {507, 120}, "US": {230, 140}, "RU": {700, 70}, "CN": {760, 160}, "NO": {525, 75}} {
		m[k] = v
	}
	return m
}()
