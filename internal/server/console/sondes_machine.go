// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	essais "mirador-stack/internal/sondes"
)

// Bloc « Sondes » de la page d'une machine (SPEC § 60) :
//   - ping de la machine depuis un collecteur de son site (ou du site
//     choisi si la machine n'en a pas) ;
//   - sondes proposées d'après les ports ouverts que l'agent a trouvés
//     (HTTPS, HTTP, SSH, TCP), ajoutées en un clic ;
//   - sondes déjà rangées sous la machine, avec leur dernier essai.
//
// Coût à l'ouverture : 2 requêtes instantanées sur les séries de la
// machine (adresses, ports) et les 4 des sondes (déjà faites par la page
// Sondes). Rien n'est mesuré en continu en plus.

type depuisSonde struct {
	Nom, Libelle string
	MemeSite     bool
}

type ligneSondeMachine struct {
	Nom, Type, Cible, Depuis string
	Etat, Classe             string
	Code                     string // « Code 200 » sous l'état (HTTP)
	CodeClasse               string // ok, info, attention, erreur
}

type propositionSonde struct {
	Valeur  string // type|cible|nom|nonverifie (relu et vérifié au retour)
	Nom     string
	Quoi    string // « HTTPS · port 443 (web) »
	Cible   string
	Deja    string // nom de la sonde qui le fait déjà
	DejaIci bool   // … rangée sous cette machine : décocher la retire
	Coche   bool
	Inconnu bool // port sans nom connu (replié)
}

// adresseMachine : adresse donnée par l'agent (SPEC § 62).
type adresseMachine struct {
	IP, Interface string
	Principale    bool   // interface de la route par défaut
	Deja          string // sonde ping qui la vise déjà
	DejaIci       bool   // … rangée sous cette machine : décocher la retire
	Coche         bool
}

type vueSondesMachine struct {
	Machine      string
	Site         string
	Sondes       []ligneSondeMachine
	Adresses     []adresseMachine
	Adresse      string // adresse des sondes proposées (route par défaut, sinon privée IPv4 d'abord)
	Depuis       []depuisSonde
	DepuisDefaut string
	SansSite     bool // la machine n'a pas de site : on choisit
	SiteSansColl bool // site sans collecteur
	NbPings      int  // sondes ping de la machine
	NbEchec      int  // sondes de la machine en échec (pastille de l'onglet)
	Propositions []propositionSonde
	AgentAncien  bool // pas d'adresses : agent à mettre à jour
}

// nomsPorts : services connus (affichage et nom de la sonde).
var nomsPorts = map[int]string{
	21: "FTP", 22: "SSH", 25: "SMTP", 53: "DNS", 80: "web", 110: "POP3", 139: "SMB", 143: "IMAP",
	389: "LDAP", 443: "web", 445: "SMB", 465: "SMTPS", 587: "SMTP", 631: "CUPS", 636: "LDAPS",
	853: "DNS TLS", 993: "IMAPS", 995: "POP3S", 1883: "MQTT", 2049: "NFS", 3000: "Grafana",
	3306: "MySQL", 3389: "RDP", 5000: "DSM", 5001: "DSM", 5432: "PostgreSQL", 5900: "VNC",
	6379: "Redis", 8006: "Proxmox", 8080: "web", 8096: "Jellyfin", 8123: "Home Assistant",
	8443: "web", 8883: "MQTT TLS", 9000: "web", 9090: "Cockpit", 9443: "Portainer",
	27017: "MongoDB", 32400: "Plex",
}

// portsSansInteret : ports qu'on ne propose pas (services système internes).
var portsSansInteret = map[int]bool{111: true}

func (c *Console) sondesMachine(ctx context.Context, m flotte.InfoMachine, releveur string, t time.Time) *vueSondesMachine {
	v := &vueSondesMachine{Machine: m.Nom, Site: m.Site, SansSite: m.Site == ""}
	// Collecteurs : ceux du site d'abord.
	cs, _ := flotte.CollecteursSites(ctx, c.db)
	for _, x := range cs {
		lib := x.Nom
		if x.Site != "" {
			lib = "site « " + x.Site + " » · collecteur " + x.Nom
		} else {
			lib = "sans site · collecteur " + x.Nom
		}
		v.Depuis = append(v.Depuis, depuisSonde{Nom: x.Nom, Libelle: lib, MemeSite: m.Site != "" && x.Site == m.Site})
	}
	sort.SliceStable(v.Depuis, func(i, j int) bool { return v.Depuis[i].MemeSite && !v.Depuis[j].MemeSite })
	switch {
	case len(v.Depuis) > 0 && v.Depuis[0].MemeSite:
		v.DepuisDefaut = v.Depuis[0].Nom
	case releveur != "":
		v.DepuisDefaut = releveur // collecteur qui relève déjà la machine
	}
	v.SiteSansColl = m.Site != "" && v.DepuisDefaut == ""
	v.Depuis = append(v.Depuis, depuisSonde{Nom: essais.ServeurLocal, Libelle: "serveur mirador (adresse publique seulement)"})

	// Adresses et ports trouvés par l'agent.
	type port struct {
		num               int
		adresse, protocol string
	}
	var ports []port
	if c.lecteur != nil {
		sel := `{instance=` + strconv.Quote(m.Nom) + `}`
		if ech, err := c.instant(ctx, `max by (adresse, interface, principale) (mirador_adresse_info`+sel+`)`, t); err == nil {
			vu := map[string]bool{}
			for _, e := range ech {
				ip := e.Etiquettes.Get("adresse")
				if !vu[ip] {
					vu[ip] = true
					v.Adresses = append(v.Adresses, adresseMachine{IP: ip, Interface: e.Etiquettes.Get("interface"),
						Principale: e.Etiquettes.Get("principale") == "1"})
				}
			}
		}
		if ech, err := c.instant(ctx, `max by (port, adresse, protocole) (mirador_port_ecoute`+sel+`)`, t); err == nil {
			for _, e := range ech {
				n, err := strconv.Atoi(e.Etiquettes.Get("port"))
				if err == nil {
					ports = append(ports, port{n, e.Etiquettes.Get("adresse"), e.Etiquettes.Get("protocole")})
				}
			}
		}
	}
	trierAdresses(v.Adresses)
	if len(v.Adresses) > 0 {
		v.Adresse = v.Adresses[0].IP
	}
	v.AgentAncien = len(v.Adresses) == 0 // agent d'avant 0.3.9 : pas d'adresses (SPEC § 62)

	// Sondes existantes : celles rangées sous la machine, et toutes les
	// cibles (une proposition déjà faite n'est pas reproposée).
	liste, _ := flotte.LireSondes(ctx, c.db)
	lues := c.sondesLues(ctx, t)
	cibles := map[string]string{}
	ici := map[string]bool{} // sondes rangées sous cette machine
	for _, s := range liste {
		cibles[cleCible(s.Type, s.Cible)] = s.Nom
		ici[s.Nom] = s.Machine == m.Nom
		if s.Machine != m.Nom {
			continue
		}
		l := ligneSondeMachine{Nom: s.Nom, Type: s.Type, Cible: s.Cible, Depuis: s.Depuis}
		var bilan int
		l.Etat, l.Classe, bilan = etatSondeLue(lues, s)
		l.Code, l.CodeClasse = codeSondeLu(lues, s)
		if bilan == bilanEchec {
			v.NbEchec++
		}
		v.Sondes = append(v.Sondes, l)
		if s.Type == essais.Ping {
			v.NbPings++
		}
	}
	// Adresses déjà pinguées (par n'importe quelle sonde) ; la première
	// adresse cochée d'office si la machine n'a encore aucun ping.
	for i := range v.Adresses {
		a := &v.Adresses[i]
		a.Deja = cibles[cleCible(essais.Ping, a.IP)]
		a.DejaIci = a.Deja != "" && ici[a.Deja]
		a.Coche = a.Deja == "" && v.NbPings == 0 && i == 0
	}

	sort.Slice(ports, func(i, j int) bool { return ports[i].num < ports[j].num })
	for _, p := range ports {
		_, connu := nomsPorts[p.num]
		// Ports système (rpcbind) et ports hauts sans nom (souvent choisis
		// au hasard au démarrage d'un service : RPC, NFS…) : pas proposés.
		if portsSansInteret[p.num] || !connu && p.num >= 32768 {
			continue
		}
		hote := p.adresse
		if hote == "*" || hote == "" {
			hote = v.Adresse
		}
		if hote == "" {
			continue
		}
		pr := proposer(m.Nom, hote, p.num, p.protocol)
		pr.Deja = cibles[cleCible(typeDeValeur(pr.Valeur), pr.Cible)]
		pr.DejaIci = pr.Deja != "" && ici[pr.Deja]
		pr.Inconnu = !connu
		pr.Coche = pr.Deja == "" && (p.protocol == "https" || p.protocol == "http" || p.protocol == "ssh")
		v.Propositions = append(v.Propositions, pr)
	}
	return v
}

// trierAdresses : route par défaut d'abord, puis IPv4 privées, IPv4,
// IPv6.
func trierAdresses(as []adresseMachine) {
	rang := func(x adresseMachine) int {
		a, err := netip.ParseAddr(x.IP)
		r := 0
		switch {
		case err != nil:
			r = 9
		case a.Is4() && a.IsPrivate():
			r = 0
		case a.Is4():
			r = 1
		case a.IsPrivate():
			r = 2
		default:
			r = 3
		}
		if !x.Principale {
			r += 10
		}
		return r
	}
	sort.SliceStable(as, func(i, j int) bool { return rang(as[i]) < rang(as[j]) })
}

// hotePort : « 192.168.1.5:443 », « [fd00::5]:443 ».
func hotePort(hote string, port int) string {
	if a, err := netip.ParseAddr(hote); err == nil {
		return netip.AddrPortFrom(a, uint16(port)).String() // #nosec G115 -- port lu d'une étiquette, 1 à 65535
	}
	return hote + ":" + strconv.Itoa(port)
}

// proposer : sonde proposée pour un port ouvert.
func proposer(machine, hote string, port int, protocole string) propositionSonde {
	nomPort := nomsPorts[port]
	quoi := strings.ToUpper(protocole)
	typ, cible, nv := essais.TCP, hotePort(hote, port), false
	switch protocole {
	case "https":
		typ, nv = essais.HTTP, true // adresse IP : le certificat ne peut pas correspondre
		cible = "https://" + hotePort(hote, port) + "/"
		if port == 443 {
			cible = "https://" + crochets(hote) + "/"
		}
	case "http":
		typ = essais.HTTP
		cible = "http://" + hotePort(hote, port) + "/"
		if port == 80 {
			cible = "http://" + crochets(hote) + "/"
		}
	case "ssh":
		quoi = "SSH"
	case "tls":
		quoi = "TLS"
	default:
		quoi = "TCP"
	}
	libelle := quoi + " · port " + strconv.Itoa(port)
	if nomPort != "" && !strings.EqualFold(nomPort, quoi) {
		libelle += " (" + nomPort + ")"
	}
	// Port toujours dans le nom : deux ports d'un même service (SMB 139
	// et 445, DSM 5000 et 5001) donnent deux noms.
	court := quoi + " " + strconv.Itoa(port)
	if nomPort != "" && nomPort != "web" && !strings.EqualFold(nomPort, quoi) {
		court = nomPort + " " + strconv.Itoa(port)
	}
	nom := nomSonde(machine, court)
	val := typ + "|" + cible + "|" + nom + "|" + strconv.FormatBool(nv)
	return propositionSonde{Valeur: val, Nom: nom, Quoi: libelle, Cible: cible}
}

func crochets(hote string) string {
	if a, err := netip.ParseAddr(hote); err == nil && a.Is6() {
		return "[" + hote + "]"
	}
	return hote
}

// nomSonde : « nas HTTPS 5001 » ; caractères hors du permis remplacés,
// 48 caractères au plus.
func nomSonde(machine, quoi string) string {
	var b strings.Builder
	for _, r := range machine {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune(" _.()'-", r), r > 127:
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	m := []rune(b.String())
	q := []rune(quoi)
	// Relecture du 21/11 : un « quoi » long (ping d'un nom d'hôte de plus de
	// 40 caractères) rendait max négatif et faisait planter la page.
	if len(q) > 40 {
		q = q[:40]
	}
	max := 47 - len(q)
	if len(m) > max {
		m = m[:max]
	}
	return strings.TrimSpace(string(m) + " " + string(q))
}

func typeDeValeur(v string) string { t, _, _ := strings.Cut(v, "|"); return t }

// cleCible : même cible (le / final ne compte pas).
func cleCible(typ, cible string) string {
	return typ + " " + strings.TrimSuffix(strings.ToLower(cible), "/")
}

// postSondesMachine : ping, ajout des propositions, retrait.
func (c *Console) postSondesMachine(q *req) {
	ctx := q.r.Context()
	nom := q.r.PathValue("nom")
	retour := "/machines/" + url.PathEscape(nom) + "#sondes"
	if q.r.PostFormValue("retour") == "sondes" { // envoyé depuis la page Sondes (§ 69)
		retour = "/sondes?ajouter=" + url.QueryEscape(nom) + "#ajouter"
	}
	if err := c.verifierCode(q); err != nil {
		c.retour(q, retour, err, "")
		return
	}
	m, ok := c.machine(q)
	if !ok {
		c.retour(q, retour, errors.New("machine introuvable"), "")
		return
	}
	liste, err := flotte.LireSondes(ctx, c.db)
	if err != nil {
		c.retour(q, retour, err, "")
		return
	}
	f := champs{q}
	depuis := f.texte("depuis")
	if depuis == "" && f.texte("action") != "retirer" {
		c.retour(q, retour, errors.New("choisir d'où faire la sonde (site et collecteur)"), "")
		return
	}
	// Sondes déjà créées pour la machine et décochées (retour du 19/11 :
	// « je ne peux pas décocher une sonde cochée ») : retirées. Chaque partie
	// du bloc a ses champs (le ping n'enlève pas les sondes proposées, et
	// l'inverse).
	decochees := func(present, garder string) map[string]bool {
		g := map[string]bool{}
		for _, n := range q.r.PostForm[garder] {
			g[n] = true
		}
		r := map[string]bool{}
		for _, n := range q.r.PostForm[present] {
			if !g[n] {
				r[n] = true
			}
		}
		return r
	}
	var retraits map[string]bool
	switch f.texte("action") {
	case "ping":
		retraits = decochees("present_a", "garder_a")
	case "ajouter":
		retraits = decochees("present_p", "garder_p")
	}
	var retirees []string
	if len(retraits) > 0 {
		garde := liste[:0:0]
		for _, x := range liste {
			if retraits[x.Nom] && x.Machine == m.Nom {
				retirees = append(retirees, "« "+x.Nom+" »")
				continue
			}
			garde = append(garde, x)
		}
		liste = garde
	}
	var ajout []essais.Sonde
	switch f.texte("action") {
	case "ping":
		// Adresses cochées (données par l'agent) et adresses tapées en plus
		// (séparées par des virgules ou des espaces) : un ping par adresse.
		adresses := append([]string{}, q.r.PostForm["adresse"]...)
		adresses = append(adresses, strings.FieldsFunc(f.texte("autre"), func(r rune) bool { return r == ',' || r == ' ' || r == ';' })...)
		existants := map[string]bool{}
		pings := 0
		for _, s := range liste {
			if s.Type == essais.Ping {
				existants[cleCible(essais.Ping, s.Cible)] = true
				if s.Machine == m.Nom {
					pings++
				}
			}
		}
		var nouvelles []string
		for _, a := range adresses {
			a = strings.TrimSpace(a)
			if a == "" || existants[cleCible(essais.Ping, a)] {
				continue
			}
			if _, err := netip.ParseAddr(a); err != nil && !nomHoteValide(a) {
				c.retour(q, retour, fmt.Errorf("adresse %q illisible", a), "")
				return
			}
			existants[cleCible(essais.Ping, a)] = true
			nouvelles = append(nouvelles, a)
		}
		if len(nouvelles) == 0 && len(retirees) == 0 {
			c.retour(q, retour, errors.New("rien à changer (cocher une adresse, en taper une, ou décocher un ping pour le retirer)"), "")
			return
		}
		for _, a := range nouvelles {
			// Une seule adresse pour la machine : « nas ping » ; plusieurs :
			// « nas ping 192.168.1.10 », « nas ping 10.10.0.1 ».
			nom := nomSonde(m.Nom, "ping")
			if pings+len(nouvelles) > 1 {
				nom = nomSonde(m.Nom, "ping "+a)
			}
			ajout = append(ajout, essais.Sonde{Nom: nom, Type: essais.Ping, Cible: a, Depuis: depuis, Machine: m.Nom})
		}
	case "ajouter":
		for _, v := range q.r.PostForm["p"] {
			p := strings.SplitN(v, "|", 4)
			if len(p) != 4 {
				continue
			}
			ajout = append(ajout, essais.Sonde{Type: p[0], Cible: p[1], Nom: p[2], NonVerifie: p[3] == "true", Depuis: depuis, Machine: m.Nom})
		}
		if len(ajout) == 0 && len(retirees) == 0 {
			c.retour(q, retour, errors.New("rien à changer (cocher une sonde à ajouter, ou décocher une sonde pour la retirer)"), "")
			return
		}
	case "retirer":
		s := f.texte("sonde")
		garde := liste[:0]
		for _, x := range liste {
			if !(x.Nom == s && x.Machine == m.Nom) {
				garde = append(garde, x)
			}
		}
		err = flotte.EnregistrerSondes(ctx, c.db, garde, q.qui(), q.t)
		c.retour(q, retour, err, "sonde « "+s+" » retirée")
		return
	default:
		c.retour(q, retour, errors.New("action inconnue"), "")
		return
	}
	// Depuis le serveur : adresses publiques seulement (le serveur refuse
	// le réseau local, SPEC § 48) ; dit tout de suite plutôt qu'au 1er essai.
	if depuis == essais.ServeurLocal {
		for _, s := range ajout {
			if adressePrivee(s) {
				c.retour(q, retour, fmt.Errorf("« %s » vise une adresse du réseau local : le serveur ne peut pas la joindre, choisir un collecteur", s.Nom), "")
				return
			}
		}
	}
	var noms []string
	for _, s := range ajout {
		noms = append(noms, "« "+s.Nom+" »")
	}
	err = flotte.EnregistrerSondes(ctx, c.db, append(liste, ajout...), q.qui(), q.t)
	var msg []string
	if len(noms) > 0 {
		msg = append(msg, strings.Join(noms, ", ")+" "+accordSimple(len(ajout), "ajoutée", "ajoutées")+" (premier essai dans la minute)")
	}
	if len(retirees) > 0 {
		msg = append(msg, strings.Join(retirees, ", ")+" "+accordSimple(len(retirees), "retirée", "retirées"))
	}
	c.retour(q, retour, err, strings.Join(msg, " ; "))
}

func accordSimple(n int, un, plusieurs string) string {
	if n > 1 {
		return plusieurs
	}
	return un
}

// adressePrivee : la cible est une adresse IP du réseau local.
func adressePrivee(s essais.Sonde) bool {
	hote := s.Cible
	if u, err := url.Parse(s.Cible); err == nil && u.Host != "" {
		hote = u.Hostname()
	} else if h, _, err := splitHotePort(s.Cible); err == nil {
		hote = h
	}
	a, err := netip.ParseAddr(hote)
	return err == nil && (a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast())
}

func splitHotePort(s string) (string, string, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return "", "", err
	}
	return ap.Addr().String(), strconv.Itoa(int(ap.Port())), nil
}

// nomHoteValide : nom d'hôte simple (lettres, chiffres, - et .).
func nomHoteValide(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}
