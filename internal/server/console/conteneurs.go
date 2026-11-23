// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/changements"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
)

// Page « Conteneurs » (SPEC.md § 42.1, refonte § 123, maquette validée le
// 23/11) : tous les conteneurs Docker du parc, rangés par machine et par
// projet Compose (ou en liste), problèmes en premier, avec la raison du
// problème (sortie du test de santé, code de sortie) et une petite courbe
// du processeur sur 24 h.
//
// Tout vient des métriques que l'agent envoie déjà (node_docker_container_*)
// et des changements notés par le serveur (image changée). Rien de plus en base.
// Coût : 12 requêtes instantanées (gardées 20 s dans le cache des requêtes),
// une requête sur 24 h pour les petites courbes (gardée 2 min), une lecture
// des changements par machine (7 jours).

// exprConteneursProblemes : conteneurs non sains, ou arrêtés alors qu'ils
// devraient redémarrer seuls (mêmes conditions que les règles
// ConteneurNonSain et ConteneurArrete). Compteur du menu.
const exprConteneursProblemes = `(count(node_docker_container_healthy == 0) or vector(0))` +
	` + on () (count(node_docker_container_running{restart_policy=~"always|unless-stopped"} == 0) or vector(0))`

// ligneConteneur : un conteneur d'une machine.
type ligneConteneur struct {
	Nom, Machine, Projet string
	Image, Politique     string
	Etat, ClasseEtat     string // En marche / Arrêté…, ok / attention / erreur
	Sante, ClasseSante   string // Sain / Non sain / "" (pas de test)
	CPU, Memoire, Limite string
	PctCPU, PctMemoire   int // -1 : pas de barre
	ClasseCPU, ClasseMem string
	Redemarrages         int
	Depuis               string // « 3 j », « 18 min » ; "" si inconnu
	DepuisS              int64  // pour le tri (secondes)
	CodeSortie           string // conteneur arrêté : « 137 » ; "" sinon
	SortieSante          string // première ligne du dernier test de santé en échec
	Ports                []string
	LienLogs             string
	Probleme, OOM        bool
	EnMarche             bool
	// Remplis pour la page seulement (completerConteneurs).
	Etincelle      string // points de la petite courbe (processeur 24 h)
	ImageChangee   string // « image changée il y a 2 j (v3.0 → v3.1) »
	Application    string
	ProblemeDepuis string // « il y a 3 h » (alerte en cours)
	rang           int
}

// Lien : fiche du conteneur.
func (l ligneConteneur) Lien() string {
	return "/conteneurs/" + url.PathEscape(l.Machine) + "/" + url.PathEscape(l.Nom)
}

// ArreteATort : arrêté alors que Docker devrait le relancer seul.
func (l ligneConteneur) ArreteATort() bool { return !l.EnMarche && l.ClasseEtat == "erreur" }

// Raison : pourquoi le conteneur est à regarder, en une phrase.
func (l ligneConteneur) Raison() string {
	var morceaux []string
	if l.ArreteATort() {
		r := "arrêté alors qu'il doit redémarrer (politique « " + l.Politique + " »)"
		if l.CodeSortie != "" {
			r += ", " + texteCodeSortie(l.CodeSortie)
		}
		morceaux = append(morceaux, r)
	}
	if l.ClasseSante == "erreur" {
		r := "non sain, test de santé en échec"
		if l.SortieSante != "" {
			r += " : « " + l.SortieSante + " »"
		}
		morceaux = append(morceaux, r)
	}
	if l.OOM {
		morceaux = append(morceaux, "tué faute de mémoire (OOM)")
	}
	return strings.Join(morceaux, " ; ")
}

// texteCodeSortie : « code de sortie 137 (tué : mémoire ou arrêt forcé) ».
func texteCodeSortie(code string) string {
	switch code {
	case "137":
		return "code de sortie 137 (tué : mémoire ou arrêt forcé)"
	case "143":
		return "code de sortie 143 (arrêté proprement)"
	}
	return "code de sortie " + code
}

// LigneErreur : petite ligne rouge sous le nom (code de sortie, sortie du
// test de santé, mémoire) ; "" s'il n'y a rien à dire.
func (l ligneConteneur) LigneErreur() string {
	switch {
	case l.CodeSortie != "" && l.CodeSortie != "0":
		return texteCodeSortie(l.CodeSortie)
	case l.ClasseSante == "erreur" && l.SortieSante != "":
		return l.SortieSante
	case l.OOM:
		return "tué faute de mémoire (OOM)"
	}
	return ""
}

// CacheFiltre : ligne cachée par le filtre de départ (lien « ?etat= »).
func (l ligneConteneur) CacheFiltre(f string) bool {
	return f != "" && !strings.Contains(" "+l.Filtres()+" ", " "+f+" ")
}

// SansTest : pas de test de santé (healthcheck) défini.
func (l ligneConteneur) SansTest() bool { return l.Sante == "" }

// Filtres : groupes de la ligne pour les filtres de la page (pb, arrete, sanstest).
func (l ligneConteneur) Filtres() string {
	var g []string
	if l.Probleme {
		g = append(g, "pb")
	}
	if !l.EnMarche {
		g = append(g, "arrete")
	}
	if l.SansTest() {
		g = append(g, "sanstest")
	}
	return strings.Join(g, " ")
}

// Texte : ce que la recherche de la page regarde.
func (l ligneConteneur) Texte() string {
	return strings.ToLower(strings.Join([]string{l.Nom, l.Machine, l.Projet, l.Image, l.Application}, " "))
}

// TexteMemoire : « 921 Mo / 3,0 Go ».
func (l ligneConteneur) TexteMemoire() string {
	if l.Limite == "" {
		return l.Memoire
	}
	return l.Memoire + " / " + l.Limite
}

// groupeConteneurs : conteneurs d'un projet Compose sur une machine.
type groupeConteneurs struct {
	Machine, Projet       string
	Lignes                []ligneConteneur
	NbNonSains, NbArretes int
	Visibles              int    // lignes montrées d'office (le reste derrière « voir »)
	Application           string // application qui reprend ce projet
	Proposer              string // lien « + créer l'application » (admin, projet sans application)
}

// NbCaches : conteneurs sains repliés.
func (g groupeConteneurs) NbCaches() int { return len(g.Lignes) - g.Visibles }

// caseConteneurs : une des 5 cases du haut de page.
type caseConteneurs struct {
	Titre, Valeur, Sur, Texte, Classe, Lien string
}

type donneesConteneurs struct {
	Lignes                                     []ligneConteneur // vue Liste, problèmes en premier
	Groupes                                    []groupeConteneurs
	Cases                                      []caseConteneurs
	Problemes                                  []ligneConteneur // bandeau du haut
	Machines                                   []string         // proposées dans le filtre
	Machine                                    string           // filtre machine (lien depuis une page machine)
	Filtre                                     string           // filtre de départ : "", pb, arrete, sanstest
	NbTous, NbProblemes, NbArretes, NbSansTest int
	NbMachines, NbProjets                      int
	SansDonnees                                bool
	Admin                                      bool
}

// lireConteneurs : un échantillon par (machine, conteneur), clé « machine\x00nom ».
func (c *Console) lireConteneurs(ctx context.Context, expr string, t time.Time) map[string]alertes.Echantillon {
	out := map[string]alertes.Echantillon{}
	ech, err := c.instant(ctx, expr, t)
	if err != nil {
		return out
	}
	for _, e := range ech {
		if math.IsNaN(e.Valeur) || math.IsInf(e.Valeur, 0) {
			continue
		}
		out[e.Etiquettes.Get("instance")+"\x00"+e.Etiquettes.Get("name")] = e
	}
	return out
}

// portsConteneurs : ports publiés par conteneur (« 8443 → 443/tcp »), triés.
func (c *Console) portsConteneurs(ctx context.Context, t time.Time) map[string][]string {
	out := map[string][]string{}
	ech, err := c.instant(ctx, `node_docker_container_port`, t)
	if err != nil {
		return out
	}
	for _, e := range ech {
		cle := e.Etiquettes.Get("instance") + "\x00" + e.Etiquettes.Get("name")
		p := e.Etiquettes.Get("public") + " → " + e.Etiquettes.Get("prive")
		if pr := e.Etiquettes.Get("proto"); pr != "" && pr != "tcp" {
			p += "/" + pr
		}
		out[cle] = append(out[cle], p)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// conteneursDuParc : toutes les lignes, problèmes en premier.
func (c *Console) conteneursDuParc(ctx context.Context, t time.Time) []ligneConteneur {
	if c.lecteur == nil {
		return nil
	}
	marche := c.lireConteneurs(ctx, `node_docker_container_running`, t)
	sante := c.lireConteneurs(ctx, `node_docker_container_healthy`, t)
	redem := c.lireConteneurs(ctx, `round(increase(node_docker_container_restart_count[24h]))`, t)
	oom := c.lireConteneurs(ctx, `node_docker_container_oom_killed`, t)
	// Processeur : part d'un cœur, comme « docker stats » (100 % = un cœur entier).
	cpu := c.lireConteneurs(ctx, `100 * rate(node_docker_container_cpu_seconds_total[5m])`, t)
	mem := c.lireConteneurs(ctx, `node_docker_container_memory_bytes`, t)
	lim := c.lireConteneurs(ctx, `node_docker_container_memory_limit_bytes`, t)
	debut := c.lireConteneurs(ctx, `node_docker_container_started_timestamp_seconds`, t)
	info := c.lireConteneurs(ctx, `node_docker_container_info`, t)
	sortie := c.lireConteneurs(ctx, `node_docker_container_exit_code`, t)
	testSante := c.lireConteneurs(ctx, `node_docker_container_health_info`, t)
	ports := c.portsConteneurs(ctx, t)

	out := make([]ligneConteneur, 0, len(marche))
	for cle, e := range marche {
		l := ligneConteneur{Nom: e.Etiquettes.Get("name"), Machine: e.Etiquettes.Get("instance"), Projet: e.Etiquettes.Get("project"),
			Politique: e.Etiquettes.Get("restart_policy"), PctCPU: -1, PctMemoire: -1, CPU: "—", Memoire: "—", Ports: ports[cle]}
		switch {
		case e.Valeur == 1:
			l.Etat, l.ClasseEtat, l.EnMarche = "En marche", "ok", true
		case l.Politique == "always" || l.Politique == "unless-stopped":
			l.Etat, l.ClasseEtat, l.Probleme = "Arrêté", "erreur", true
		default:
			l.Etat, l.ClasseEtat = etatConteneur(e.Etiquettes.Get("state")), ""
		}
		if i, ok := info[cle]; ok {
			l.Image = i.Etiquettes.Get("image")
		}
		if s, ok := sortie[cle]; ok && !l.EnMarche {
			l.CodeSortie = strconv.FormatFloat(s.Valeur, 'f', 0, 64)
		}
		if s, ok := sante[cle]; ok {
			if s.Valeur == 1 {
				l.Sante, l.ClasseSante = "Sain", "ok"
			} else {
				l.Sante, l.ClasseSante, l.Probleme = "Non sain", "erreur", true
				if h, ok := testSante[cle]; ok {
					l.SortieSante = h.Etiquettes.Get("sortie")
				}
			}
		}
		if o, ok := oom[cle]; ok && o.Valeur == 1 {
			l.OOM, l.Probleme = true, true
		}
		if r, ok := redem[cle]; ok && r.Valeur > 0 {
			l.Redemarrages = int(r.Valeur)
		}
		if v, ok := cpu[cle]; ok && l.EnMarche {
			l.CPU, l.PctCPU = virgule(formatPct(v.Valeur)), pctBarre(v.Valeur)
			l.ClasseCPU = classeSeuil(v.Valeur, 90)
		}
		if v, ok := mem[cle]; ok && l.EnMarche {
			l.Memoire = virgule(tailleLisible(int64(v.Valeur)))
			if m, ok := lim[cle]; ok && m.Valeur > 0 {
				p := 100 * v.Valeur / m.Valeur
				l.Limite = virgule(tailleLisible(int64(m.Valeur)))
				l.PctMemoire, l.ClasseMem = pctBarre(p), classeSeuil(p, 90)
			}
		}
		if d, ok := debut[cle]; ok && d.Valeur > 0 && l.EnMarche {
			duree := t.Sub(time.Unix(int64(d.Valeur), 0))
			if duree > 0 {
				l.Depuis, l.DepuisS = dureeLisible(duree), int64(duree.Seconds())
			}
		}
		q := url.Values{"host": {l.Machine}, "container": {l.Nom}}
		l.LienLogs = "/logs?" + q.Encode()
		out = append(out, l)
	}
	trierConteneurs(out)
	return out
}

// trierConteneurs : problèmes d'abord, puis machine, puis nom.
func trierConteneurs(ls []ligneConteneur) {
	sort.Slice(ls, func(i, j int) bool {
		a, b := ls[i], ls[j]
		if a.Probleme != b.Probleme {
			return a.Probleme
		}
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		return a.Nom < b.Nom
	})
}

// etatConteneur : état Docker en français.
func etatConteneur(s string) string {
	switch s {
	case "exited":
		return "Arrêté"
	case "created":
		return "Créé"
	case "paused":
		return "En pause"
	case "restarting":
		return "Redémarre"
	case "dead":
		return "Mort"
	case "removing":
		return "Suppression"
	}
	if s == "" {
		return "—"
	}
	return s
}

// formatPct : « 22 % », « 0.4 % » (virgule mise ensuite).
func formatPct(v float64) string {
	if v > 0 && v < 10 {
		return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + " %"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " %"
}

// nbConteneursProblemes : compteur du menu (0 sans métriques).
func (c *Console) nbConteneursProblemes(ctx context.Context, t time.Time) int {
	if c.lecteur == nil {
		return 0
	}
	ech, err := c.instant(ctx, exprConteneursProblemes, t)
	if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) {
		return 0
	}
	return int(ech[0].Valeur)
}

// etincellesConteneurs : petite courbe du processeur sur 24 h pour chaque
// conteneur (une seule requête pour tout le parc, une valeur par 30 min,
// gardée 2 min). Échelle propre à chaque conteneur, 5 % au moins pour
// qu'un conteneur au repos reste plat.
func (c *Console) etincellesConteneurs(ctx context.Context, t time.Time) map[string]string {
	c.muEtincellesCt.Lock()
	defer c.muEtincellesCt.Unlock()
	if t.Before(c.etincellesCtExpire) {
		return c.etincellesCtCache
	}
	out := map[string]string{}
	if c.lecteur != nil {
		fin := t.Truncate(10 * time.Minute)
		debut := fin.Add(-24 * time.Hour)
		if courbes, _, err := c.lecteur.Plage(ctx, `100 * rate(node_docker_container_cpu_seconds_total[30m])`, debut, fin, 30*time.Minute); err == nil {
			for _, cb := range courbes {
				haut := 5.0
				for _, v := range cb.V {
					if !math.IsNaN(v) && v > haut {
						haut = v
					}
				}
				var b strings.Builder
				for i, ts := range cb.T {
					if math.IsNaN(cb.V[i]) {
						continue
					}
					x := float64(ts-debut.UnixMilli()) / float64(24*time.Hour/time.Millisecond) * 90
					y := 23 - math.Max(0, cb.V[i])/haut*22
					if b.Len() > 0 {
						b.WriteByte(' ')
					}
					fmt.Fprintf(&b, "%.1f,%.1f", x, y)
				}
				if strings.Contains(b.String(), " ") { // deux points au moins
					out[cb.Etiquettes.Get("instance")+"\x00"+cb.Etiquettes.Get("name")] = b.String()
				}
			}
		}
	}
	c.etincellesCtCache, c.etincellesCtExpire = out, t.Add(2*time.Minute)
	return out
}

// imagesChangees : dernière image changée (7 jours) par conteneur, d'après
// les changements notés par le serveur (« nom recréé : image A → B »).
func (c *Console) imagesChangees(ctx context.Context, machines []string, t time.Time) map[string]string {
	out := map[string]string{}
	for _, m := range machines {
		chg, err := changements.Lister(ctx, c.db, m, t.Add(-7*24*time.Hour), 300)
		if err != nil {
			continue
		}
		for _, x := range chg { // les plus récents d'abord
			if x.Type != changements.Conteneur {
				continue
			}
			nom, images, ok := strings.Cut(x.Detail, " recréé : image ")
			if !ok || out[m+"\x00"+nom] != "" {
				continue
			}
			out[m+"\x00"+nom] = "image changée il y a " + dureeLisible(t.Sub(x.Le)) + " (" + images + ")"
		}
	}
	return out
}

// appsConteneurs : application qui reprend chaque conteneur (« machine/nom »).
func (c *Console) appsConteneurs(ctx context.Context) map[string]string {
	out := map[string]string{}
	svcs, err := servicesmetier.Lire(ctx, c.db)
	if err != nil {
		return out
	}
	for _, s := range svcs {
		for _, x := range s.Composants {
			if x.Sorte == servicesmetier.Conteneur && out[x.Nom] == "" {
				out[x.Nom] = s.Nom
			}
		}
	}
	return out
}

// problemesDepuis : début de l'alerte en cours (non sain, arrêté) par conteneur.
func (c *Console) problemesDepuis() map[string]time.Time {
	out := map[string]time.Time{}
	if c.opt.Alertes == nil {
		return out
	}
	for _, a := range c.opt.Alertes.EnCours() {
		if a.Etat != alertes.Active || (a.Regle != "ConteneurNonSain" && a.Regle != "ConteneurArrete") {
			continue
		}
		cle := a.Machine() + "\x00" + a.Etiquettes.Get("name")
		if d, ok := out[cle]; !ok || a.Debut.Before(d) {
			out[cle] = a.Debut
		}
	}
	return out
}

// completerConteneurs : ce que seule la page (et la fiche) montre : petite
// courbe, image changée, application, depuis quand le problème dure.
func (c *Console) completerConteneurs(ctx context.Context, ls []ligneConteneur, t time.Time) {
	etinc := c.etincellesConteneurs(ctx, t)
	vues := map[string]bool{}
	var machines []string
	for _, l := range ls {
		if !vues[l.Machine] {
			vues[l.Machine] = true
			machines = append(machines, l.Machine)
		}
	}
	images := c.imagesChangees(ctx, machines, t)
	apps := c.appsConteneurs(ctx)
	depuisPb := c.problemesDepuis()
	for i := range ls {
		l := &ls[i]
		cle := l.Machine + "\x00" + l.Nom
		if l.EnMarche {
			l.Etincelle = etinc[cle]
		}
		l.ImageChangee = images[cle]
		l.Application = apps[l.Machine+"/"+l.Nom]
		if d, ok := depuisPb[cle]; ok && l.Probleme {
			l.ProblemeDepuis = "il y a " + dureeLisible(t.Sub(d))
		}
	}
}

// regrouperConteneurs : un groupe par (machine, projet Compose) ; groupes
// avec un problème d'abord. Un groupe sans problème de plus de 3
// conteneurs n'en montre que 2, le reste derrière « voir ».
func regrouperConteneurs(ls []ligneConteneur, admin bool) []groupeConteneurs {
	par := map[string]*groupeConteneurs{}
	var ordre []string
	for _, l := range ls {
		cle := l.Machine + "\x00" + l.Projet
		g := par[cle]
		if g == nil {
			g = &groupeConteneurs{Machine: l.Machine, Projet: l.Projet}
			par[cle] = g
			ordre = append(ordre, cle)
		}
		g.Lignes = append(g.Lignes, l)
		if l.ClasseSante == "erreur" {
			g.NbNonSains++
		}
		if l.ArreteATort() {
			g.NbArretes++
		}
		if g.Application == "" {
			g.Application = l.Application
		}
	}
	out := make([]groupeConteneurs, 0, len(ordre))
	for _, cle := range ordre {
		g := par[cle]
		trierConteneurs(g.Lignes)
		g.Visibles = len(g.Lignes)
		if !g.AProbleme() && len(g.Lignes) > 3 {
			g.Visibles = 2
		}
		if admin && g.Projet != "" && g.Application == "" {
			g.Proposer = "/applications?" + url.Values{"proposer": {g.Projet}, "machine": {g.Machine}}.Encode() + "#editeur"
		}
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.AProbleme() != b.AProbleme() {
			return a.AProbleme()
		}
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		if (a.Projet == "") != (b.Projet == "") {
			return b.Projet == "" // conteneurs sans projet en dernier
		}
		return a.Projet < b.Projet
	})
	return out
}

// CacheFiltre : groupe dont toutes les lignes sont cachées par le filtre de départ.
func (g groupeConteneurs) CacheFiltre(f string) bool {
	for _, l := range g.Lignes {
		if !l.CacheFiltre(f) {
			return false
		}
	}
	return true
}

// AProbleme : au moins un conteneur du groupe à regarder.
func (g groupeConteneurs) AProbleme() bool {
	for _, l := range g.Lignes {
		if l.Probleme {
			return true
		}
	}
	return false
}

// listeNoms : « appli, cache, web… » (3 noms au plus).
func listeNoms(noms []string) string {
	if len(noms) > 3 {
		return strings.Join(noms[:3], ", ") + "…"
	}
	return strings.Join(noms, ", ")
}

// casesConteneurs : les 5 cases du haut de page.
func casesConteneurs(ls []ligneConteneur, nbMachines, nbProjets int) []caseConteneurs {
	var marche int
	var nonSains, arretes, redem, oom []string
	for _, l := range ls {
		if l.EnMarche {
			marche++
		}
		if l.ClasseSante == "erreur" {
			nonSains = append(nonSains, l.Nom)
		}
		if l.ArreteATort() {
			arretes = append(arretes, l.Nom+" ("+l.Politique+")")
		}
		if l.Redemarrages > 0 {
			n := l.Nom
			if l.Redemarrages > 1 {
				n += " ×" + strconv.Itoa(l.Redemarrages)
			}
			redem = append(redem, n)
		}
		if l.OOM {
			oom = append(oom, l.Nom)
		}
	}
	sous := strconv.Itoa(nbMachines) + " " + accorder(nbMachines, "machine(s)")
	if nbProjets > 0 {
		sous += " · " + strconv.Itoa(nbProjets) + " " + accorder(nbProjets, "projet(s)")
	}
	ou := func(noms []string, sinon string) string {
		if len(noms) == 0 {
			return sinon
		}
		return listeNoms(noms)
	}
	classe := func(n int, c string) string {
		if n > 0 {
			return c
		}
		return ""
	}
	return []caseConteneurs{
		{Titre: "En marche", Valeur: strconv.Itoa(marche), Sur: strconv.Itoa(len(ls)), Texte: "sur " + sous, Lien: "/conteneurs"},
		{Titre: "Non sains", Valeur: strconv.Itoa(len(nonSains)), Texte: ou(nonSains, "aucun"), Classe: classe(len(nonSains), "erreur"), Lien: "/conteneurs?etat=problemes"},
		{Titre: "Arrêtés à tort", Valeur: strconv.Itoa(len(arretes)), Texte: ou(arretes, "aucun"), Classe: classe(len(arretes), "attention"), Lien: "/conteneurs?etat=arretes"},
		{Titre: "Redémarrés 24 h", Valeur: strconv.Itoa(len(redem)), Texte: ou(redem, "aucun"), Lien: "/conteneurs"},
		{Titre: "Mémoire pleine (OOM)", Valeur: strconv.Itoa(len(oom)), Texte: ou(oom, "aucun"), Classe: classe(len(oom), "erreur"), Lien: "/conteneurs?etat=problemes"},
	}
}

func (c *Console) pageConteneurs(q *req) {
	ctx := q.r.Context()
	d := donneesConteneurs{Machine: q.r.URL.Query().Get("machine"), Admin: q.admin()}
	switch q.r.URL.Query().Get("etat") {
	case "problemes":
		d.Filtre = "pb"
	case "arretes":
		d.Filtre = "arrete"
	case "sanstest":
		d.Filtre = "sanstest"
	}
	toutes := c.conteneursDuParc(ctx, q.t)
	connues := map[string]bool{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if m.Statut == "approuvee" {
				connues[m.Nom] = true
			}
		}
	}
	vues := map[string]bool{}
	for _, l := range toutes {
		if len(connues) > 0 && !connues[l.Machine] {
			continue // machine supprimée depuis : séries encore en mémoire
		}
		vues[l.Machine] = true
		if d.Machine != "" && l.Machine != d.Machine {
			continue
		}
		d.Lignes = append(d.Lignes, l)
	}
	for m := range vues {
		d.Machines = append(d.Machines, m)
	}
	sort.Strings(d.Machines)
	c.completerConteneurs(ctx, d.Lignes, q.t)
	projets := map[string]bool{}
	machinesVues := map[string]bool{}
	for _, l := range d.Lignes {
		machinesVues[l.Machine] = true
		if l.Projet != "" {
			projets[l.Machine+"\x00"+l.Projet] = true
		}
		if l.Probleme {
			d.NbProblemes++
			d.Problemes = append(d.Problemes, l)
		}
		if !l.EnMarche {
			d.NbArretes++
		}
		if l.SansTest() {
			d.NbSansTest++
		}
	}
	d.NbTous, d.NbMachines, d.NbProjets = len(d.Lignes), len(machinesVues), len(projets)
	d.Groupes = regrouperConteneurs(d.Lignes, d.Admin)
	d.Cases = casesConteneurs(d.Lignes, d.NbMachines, d.NbProjets)
	d.SansDonnees = len(toutes) == 0
	c.rendre(q, "conteneurs", "Conteneurs", "conteneurs", d)
}
