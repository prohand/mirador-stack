// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
)

// Surveillance par machine (SPEC.md § 19, idée reprise de Beszel) : la page
// d'une machine montre ce que l'agent a trouvé, ce qui est surveillé, et
// laisse valider, ignorer ou retirer chaque élément.
//
// Rien n'est stocké en plus : l'état se déduit des dernières métriques de la
// machine (une requête, gardée en cache) et de sa configuration distante. Un
// choix n'écrit qu'une clé typée de cette configuration (jamais de commande).

// genreSurv : un élément du catalogue. Cle est la clé (booléenne) de la
// configuration distante de l'agent.
type genreSurv struct {
	Cle, Titre, Propose string
	Nouveau             bool // éteint par défaut : proposé seulement s'il est détecté
}

var catalogueSurv = []genreSurv{
	{Cle: "metriques", Titre: "Système"},
	{Cle: "temperatures", Titre: "Températures"},
	{Cle: "ventilateurs", Titre: "Ventilateurs", Nouveau: true, Propose: "vitesse de chaque ventilateur ; alerte si l'un s'arrête"},
	{Cle: "zfs", Titre: "Pools ZFS", Nouveau: true, Propose: "santé de chaque pool ; alerte critique s'il n'est plus ONLINE"},
	{Cle: "mdraid", Titre: "RAID logiciel", Nouveau: true, Propose: "disques manquants ; alerte critique si une grappe est dégradée"},
	{Cle: "envoyer_smart", Titre: "Disques SMART"},
	{Cle: "envoyer_pare_feu", Titre: "Pare-feu"},
	{Cle: "envoyer_certificats", Titre: "Certificats"},
	{Cle: "docker_metriques", Titre: "Conteneurs Docker"},
	{Cle: "systemd", Titre: "Services systemd"},
	{Cle: "mises_a_jour", Titre: "Mises à jour"},
	{Cle: "journal", Titre: "Journal système"},
	{Cle: "logs_docker", Titre: "Logs des conteneurs"},
}

// États d'un élément.
const (
	survSurveille = "surveille"
	survAValider  = "a_valider"
	survIgnore    = "ignore"
	survAbsent    = "absent"
)

// ligneSurv : un élément du catalogue pour une machine.
type ligneSurv struct {
	Cle, Titre, Detail, Propose string
	Etat                        string
	Alerte                      bool // quelque chose ne va pas (pool dégradé, disque en échec…)
	Bloque                      bool // agent trop ancien pour ce réglage
}

// serviceSurv : un service systemd.
type serviceSurv struct {
	Nom    string
	Arrete bool
}

// vueSurveillance : le bloc « À surveiller sur cette machine ».
type vueSurveillance struct {
	Capable                                bool // l'agent connaît ces réglages
	Groupe                                 string
	AValider, Surveilles, Ignores          []ligneSurv
	Absents                                []string
	Validation                             bool // services validés un par un
	SvcAValider, SvcSurveilles, SvcIgnores []serviceSurv
	NbAValider, NbSurveilles, NbIgnores    int
}

// metriquesSurv : tout ce qu'il faut lire pour une machine, en une requête.
const metriquesSurv = `mirador_agent_detecte|node_hwmon_temp_celsius|node_thermal_zone_temp|node_hwmon_fan_rpm|` +
	`node_zfs_zpool_state|node_md_degraded|smartctl_device_smart_status|node_firewall_input_policy_drop|` +
	`node_firewall_input_rules|ssl_certs_found_total|node_docker_up|node_docker_container_running|` +
	`node_systemd_enabled_units_total|node_systemd_enabled_units_down|node_systemd_unit_enabled_active|` +
	`apt_upgrades_pending|mirador_logs_journal_lines_read_total|mirador_logs_docker_streams|mirador_agent_info|` +
	`node_uname_info|node_memory_MemTotal_bytes|node_boot_time_seconds`

func exprSurv(machine string) string {
	return fmt.Sprintf(`{instance=%q,__name__=~%q}`, machine, metriquesSurv)
}

// valeurSurv : valeur d'une clé du catalogue dans la configuration effective
// (nil = défaut de l'agent).
func valeurSurv(c *distant.ConfigDistante, cle string) *bool {
	switch cle {
	case "metriques":
		return c.Metriques
	case "temperatures":
		return c.Temperatures
	case "ventilateurs":
		return c.Ventilateurs
	case "zfs":
		return c.ZFS
	case "mdraid":
		return c.Mdraid
	case "envoyer_smart":
		return c.EnvoyerSmart
	case "envoyer_pare_feu":
		return c.EnvoyerPareFeu
	case "envoyer_certificats":
		return c.EnvoyerCertificats
	case "docker_metriques":
		return c.DockerMetriques
	case "systemd":
		return c.Systemd
	case "mises_a_jour":
		return c.MisesAJour
	case "journal":
		return c.Journal
	case "logs_docker":
		return c.LogsDocker
	}
	return nil
}

// releve : les métriques d'une machine, rangées par usage.
type releve struct {
	detecte             map[string][]string // genre → noms
	nbFansVus           int
	temps               int
	tempMax             float64
	fans, fans0         int
	pools               map[string]string // pool → état
	grappes             map[string]float64
	smart, smartKO      int
	pareFeu             *float64
	regles              float64
	certs               *float64
	dockerUp            *float64
	conteneurs          int
	svcTotal, svcDown   *float64
	services            map[string]bool // service → tourne
	majs                *float64
	journal, logsDocker *float64
	agent               bool
}

func lireReleve(ech []alertes.Echantillon) releve {
	r := releve{detecte: map[string][]string{}, pools: map[string]string{}, grappes: map[string]float64{}, services: map[string]bool{}}
	for i := range ech {
		e := &ech[i]
		v := e.Valeur
		etq := e.Etiquettes.Get
		switch etq("__name__") {
		case "mirador_agent_detecte":
			g := etq("genre")
			r.detecte[g] = append(r.detecte[g], etq("nom"))
			if g == "ventilateurs" {
				r.nbFansVus = int(v)
			}
		case "node_hwmon_temp_celsius", "node_thermal_zone_temp":
			r.temps++
			r.tempMax = max(r.tempMax, v)
		case "node_hwmon_fan_rpm":
			r.fans++
			if v == 0 {
				r.fans0++
			}
		case "node_zfs_zpool_state":
			pool, etat := etq("zpool"), etq("state")
			if etat != "online" || v == 1 {
				r.pools[pool] = etat // l'état courant (online seulement s'il vaut 1)
			} else if _, vu := r.pools[pool]; !vu {
				r.pools[pool] = "?"
			}
		case "node_md_degraded":
			r.grappes[etq("device")] = v
		case "smartctl_device_smart_status":
			r.smart++
			if v == 0 {
				r.smartKO++
			}
		case "node_firewall_input_policy_drop":
			r.pareFeu = &e.Valeur
		case "node_firewall_input_rules":
			r.regles = v
		case "ssl_certs_found_total":
			r.certs = &e.Valeur
		case "node_docker_up":
			r.dockerUp = &e.Valeur
		case "node_docker_container_running":
			r.conteneurs++
		case "node_systemd_enabled_units_total":
			r.svcTotal = &e.Valeur
		case "node_systemd_enabled_units_down":
			r.svcDown = &e.Valeur
		case "node_systemd_unit_enabled_active":
			r.services[etq("name")] = v == 1
		case "apt_upgrades_pending":
			r.majs = &e.Valeur
		case "mirador_logs_journal_lines_read_total":
			r.journal = &e.Valeur
		case "mirador_logs_docker_streams":
			r.logsDocker = &e.Valeur
		case "mirador_agent_info":
			r.agent = true
		}
	}
	for _, l := range r.detecte {
		sort.Strings(l)
	}
	return r
}

// pluriel : « 1 disque », « 3 disques ».
func pluriel(n int, mot string) string {
	if n > 1 {
		mot += "s"
	}
	return fmt.Sprintf("%d %s", n, mot)
}

// detailSurv : pour un élément, s'il est présent sur la machine, ce qu'on en
// dit, et si quelque chose ne va pas.
func (r *releve) detailSurv(cle string) (present bool, detail string, alerte bool) {
	switch cle {
	case "metriques":
		return r.agent, "Processeur, mémoire, disques, réseau", false
	case "temperatures":
		if r.temps == 0 {
			return false, "", false
		}
		return true, fmt.Sprintf("%s, %.0f °C au plus", pluriel(r.temps, "capteur"), r.tempMax), false
	case "ventilateurs":
		if r.fans > 0 {
			d := pluriel(r.fans, "ventilateur")
			if r.fans0 > 0 {
				d += fmt.Sprintf(", %d à l'arrêt", r.fans0)
			}
			return true, d, r.fans0 > 0
		}
		if r.nbFansVus > 0 {
			return true, pluriel(r.nbFansVus, "capteur") + " trouvé(s)", false
		}
	case "zfs":
		if len(r.pools) > 0 {
			var noms []string
			for p, etat := range r.pools {
				noms = append(noms, p+" ("+strings.ToUpper(etat)+")")
				alerte = alerte || etat != "online"
			}
			sort.Strings(noms)
			return true, strings.Join(noms, ", "), alerte
		}
		if n := r.detecte["zfs"]; len(n) > 0 {
			return true, strings.Join(n, ", "), false
		}
	case "mdraid":
		if len(r.grappes) > 0 {
			var noms []string
			for g, manquants := range r.grappes {
				if manquants > 0 {
					noms = append(noms, fmt.Sprintf("%s (%.0f disque(s) manquant(s))", g, manquants))
					alerte = true
				} else {
					noms = append(noms, g+" (saine)")
				}
			}
			sort.Strings(noms)
			return true, strings.Join(noms, ", "), alerte
		}
		if n := r.detecte["mdraid"]; len(n) > 0 {
			return true, strings.Join(n, ", "), false
		}
	case "envoyer_smart":
		if r.smart == 0 {
			return false, "", false
		}
		if r.smartKO > 0 {
			return true, fmt.Sprintf("%s, %d en échec", pluriel(r.smart, "disque"), r.smartKO), true
		}
		return true, pluriel(r.smart, "disque") + ", tous sains", false
	case "envoyer_pare_feu":
		if r.pareFeu == nil {
			return false, "", false
		}
		if *r.pareFeu == 1 {
			return true, fmt.Sprintf("Entrées bloquées par défaut, %s", pluriel(int(r.regles), "règle")), false
		}
		return true, "Entrées non bloquées par défaut", true
	case "envoyer_certificats":
		if r.certs == nil {
			return false, "", false
		}
		return true, pluriel(int(*r.certs), "certificat") + " suivi(s)", false
	case "docker_metriques":
		if r.dockerUp == nil {
			return false, "", false
		}
		if *r.dockerUp == 0 {
			return true, "Docker ne répond pas", true
		}
		return true, pluriel(r.conteneurs, "conteneur"), false
	case "systemd":
		if r.svcTotal == nil {
			return false, "", false
		}
		d := pluriel(int(*r.svcTotal), "service") + " suivi(s)"
		if r.svcDown != nil && *r.svcDown > 0 {
			return true, d + fmt.Sprintf(", %.0f arrêté(s)", *r.svcDown), true
		}
		return true, d + ", tous actifs", false
	case "mises_a_jour":
		if r.majs == nil {
			return false, "", false
		}
		return true, pluriel(int(*r.majs), "paquet") + " en attente", false
	case "journal":
		return r.journal != nil, "Logs du système envoyés", false
	case "logs_docker":
		if r.logsDocker == nil {
			return false, "", false
		}
		return true, pluriel(int(*r.logsDocker), "conteneur") + " suivi(s)", false
	}
	return false, "", false
}

// classerSurveillance : état de chaque élément pour une machine, d'après sa
// configuration effective et ses dernières métriques.
func classerSurveillance(cfg *distant.ConfigDistante, ech []alertes.Echantillon, capable bool) vueSurveillance {
	r := lireReleve(ech)
	v := vueSurveillance{Capable: capable}
	for _, g := range catalogueSurv {
		present, detail, alerte := r.detailSurv(g.Cle)
		l := ligneSurv{Cle: g.Cle, Titre: g.Titre, Detail: detail, Propose: g.Propose, Alerte: alerte,
			Bloque: !capable && flotte.CleSurveillance(g.Cle)}
		choix := valeurSurv(cfg, g.Cle)
		switch {
		case choix != nil && !*choix:
			l.Etat, l.Alerte = survIgnore, false
			v.Ignores = append(v.Ignores, l)
		case g.Nouveau && choix == nil:
			if !present {
				v.Absents = append(v.Absents, g.Titre)
				continue
			}
			l.Etat = survAValider
			v.AValider = append(v.AValider, l)
		case !present:
			v.Absents = append(v.Absents, g.Titre)
		default:
			l.Etat = survSurveille
			v.Surveilles = append(v.Surveilles, l)
		}
	}
	// Services systemd, un par un.
	v.Validation = cfg.SystemdValidation != nil && *cfg.SystemdValidation
	systemdActif := cfg.Systemd == nil || *cfg.Systemd
	if systemdActif {
		vus := map[string]bool{}
		if v.Validation && cfg.SystemdSurveiller != nil {
			for _, n := range *cfg.SystemdSurveiller {
				tourne, connu := r.services[n]
				v.SvcSurveilles = append(v.SvcSurveilles, serviceSurv{Nom: n, Arrete: connu && !tourne})
				vus[n] = true
			}
		}
		if !v.Validation {
			for n, tourne := range r.services {
				v.SvcSurveilles = append(v.SvcSurveilles, serviceSurv{Nom: n, Arrete: !tourne})
			}
		}
		for _, n := range r.detecte["service"] {
			if !vus[n] {
				v.SvcAValider = append(v.SvcAValider, serviceSurv{Nom: n})
			}
		}
		if cfg.SystemdIgnorer != nil {
			for _, n := range *cfg.SystemdIgnorer {
				v.SvcIgnores = append(v.SvcIgnores, serviceSurv{Nom: n})
			}
		}
		for _, l := range [][]serviceSurv{v.SvcAValider, v.SvcSurveilles, v.SvcIgnores} {
			sort.Slice(l, func(i, j int) bool { return l[i].Nom < l[j].Nom })
		}
	}
	v.NbAValider = len(v.AValider) + len(v.SvcAValider)
	v.NbSurveilles = len(v.Surveilles)
	v.NbIgnores = len(v.Ignores) + len(v.SvcIgnores)
	return v
}

// surveillance : le bloc d'une machine (vide si les métriques manquent).
func (c *Console) surveillance(ctx context.Context, m flotte.InfoMachine, t time.Time) (vueSurveillance, []alertes.Echantillon) {
	cfg, err := flotte.ConfigEffective(ctx, c.db, m.ID)
	if err != nil {
		cfg = &distant.ConfigDistante{}
	}
	ech, _ := c.instant(ctx, exprSurv(m.Nom), t)
	v := classerSurveillance(cfg, ech, m.Surveillance())
	v.Groupe = m.Groupe
	return v, ech
}

// nouveautes : nombre d'éléments « à valider » par machine, pour la vue
// d'ensemble (une requête pour tout le parc).
func (c *Console) nouveautes(ctx context.Context, ms []flotte.InfoMachine, t time.Time) map[string]int {
	ech, err := c.instant(ctx, `count by (instance, genre) (mirador_agent_detecte)`, t)
	if err != nil || len(ech) == 0 {
		return nil
	}
	ids := map[string]int64{}
	for _, m := range ms {
		ids[m.Nom] = m.ID
	}
	configs := map[string]*distant.ConfigDistante{}
	out := map[string]int{}
	for _, e := range ech {
		nom, genre := e.Etiquettes.Get("instance"), e.Etiquettes.Get("genre")
		if genre == "service" {
			out[nom] += int(e.Valeur)
			continue
		}
		// Ventilateurs, ZFS, RAID : déjà refusé pour cette machine ?
		cfg, lu := configs[nom]
		if !lu {
			if id, ok := ids[nom]; ok {
				cfg, _ = flotte.ConfigEffective(ctx, c.db, id)
			}
			configs[nom] = cfg
		}
		if cfg == nil || valeurSurv(cfg, genre) == nil {
			out[nom]++
		}
	}
	return out
}

// --- Enregistrement des choix ---------------------------------------------------------

var formatService = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]{1,255}\.service$`)

// postSurveillance applique les choix du formulaire du bloc « À surveiller ».
// Champs : g.CLE = oui | non | defaut ; s.SERVICE = surveiller | ignorer |
// retablir ; validation = oui | non ; tout = oui | non (tout ce qui est « à
// valider ») ; portee = machine | groupe (éléments du catalogue seulement).
func (c *Console) postSurveillance(q *req, m flotte.InfoMachine) error {
	ctx := q.r.Context()
	f := q.r.PostFormValue
	vue, ech := c.surveillance(ctx, m, q.t)
	cfg, err := flotte.ConfigEffective(ctx, c.db, m.ID)
	if err != nil {
		return err
	}
	tout := f("tout")

	// Éléments du catalogue.
	genres := map[string]any{}
	for _, g := range catalogueSurv {
		choix := f("g." + g.Cle)
		if choix == "" && tout != "" {
			for _, l := range vue.AValider {
				if l.Cle == g.Cle {
					choix = tout
				}
			}
		}
		switch choix {
		case "":
			continue
		case "oui":
			genres[g.Cle] = true
		case "non":
			genres[g.Cle] = false
		case "defaut":
			genres[g.Cle] = nil
		default:
			return errors.New("choix inconnu")
		}
		if flotte.CleSurveillance(g.Cle) && !m.Surveillance() {
			return fmt.Errorf("« %s » demande un agent plus récent sur %s : le mettre à jour d'abord", g.Titre, m.Nom)
		}
	}

	// Services systemd (toujours à la portée de la machine).
	liste := func(p *[]string) map[string]bool {
		out := map[string]bool{}
		if p != nil {
			for _, n := range *p {
				out[n] = true
			}
		}
		return out
	}
	surveiller, ignorer := liste(cfg.SystemdSurveiller), liste(cfg.SystemdIgnorer)
	validation := vue.Validation
	services := false
	switch f("validation") {
	case "oui":
		if !m.Surveillance() {
			return fmt.Errorf("la validation des services demande un agent plus récent sur %s", m.Nom)
		}
		// Point de départ : ce qui est surveillé aujourd'hui reste surveillé.
		r := lireReleve(ech)
		surveiller = map[string]bool{}
		for n := range r.services {
			if !ignorer[n] {
				surveiller[n] = true
			}
		}
		validation, services = true, true
	case "non":
		validation, services = false, true
		surveiller = map[string]bool{}
	}
	choixService := func(nom, choix string) error {
		if !formatService.MatchString(nom) {
			return fmt.Errorf("nom de service invalide %q", nom)
		}
		switch choix {
		case "surveiller", "retablir", "oui":
			delete(ignorer, nom)
			if validation {
				surveiller[nom] = true
			}
		case "ignorer", "non":
			delete(surveiller, nom)
			ignorer[nom] = true
		case "":
			return nil
		default:
			return errors.New("choix inconnu")
		}
		services = true
		return nil
	}
	n := 0
	for champ, valeurs := range q.r.PostForm {
		nom, ok := strings.CutPrefix(champ, "s.")
		if !ok || len(valeurs) == 0 {
			continue
		}
		if n++; n > 400 {
			return errors.New("trop de services dans la demande")
		}
		if err := choixService(nom, valeurs[0]); err != nil {
			return err
		}
	}
	if tout != "" {
		for _, s := range vue.SvcAValider {
			if f("s."+s.Nom) == "" {
				if err := choixService(s.Nom, tout); err != nil {
					return err
				}
			}
		}
	}
	if len(genres) == 0 && !services {
		return errors.New("aucun choix à enregistrer")
	}
	trie := func(e map[string]bool) any {
		if len(e) == 0 {
			return nil // clé retirée
		}
		out := make([]string, 0, len(e))
		for n := range e {
			out = append(out, n)
		}
		sort.Strings(out)
		return out
	}
	ecrireServices := func(doc map[string]any) {
		if !services {
			return
		}
		doc["systemd_ignorer"] = trie(ignorer)
		if validation {
			doc["systemd_validation"] = true
			// Liste vide gardée : « aucun service validé » n'est pas « tous ».
			l, _ := trie(surveiller).([]string)
			if l == nil {
				l = []string{}
			}
			doc["systemd_surveiller"] = l
		} else {
			doc["systemd_validation"], doc["systemd_surveiller"] = nil, nil
		}
	}
	porteeMachine := fmt.Sprintf("machine:%d", m.ID)
	if f("portee") == "groupe" && m.Groupe != "" && len(genres) > 0 {
		// Le choix va au groupe ; il est retiré de la machine pour ne pas le masquer.
		err = flotte.ModifierConfig(ctx, c.db, "groupe:"+m.Groupe, func(doc map[string]any) {
			for k, v := range genres {
				doc[k] = v
			}
		}, q.qui(), q.t)
		if err != nil {
			return err
		}
		return flotte.ModifierConfig(ctx, c.db, porteeMachine, func(doc map[string]any) {
			for k := range genres {
				doc[k] = nil
			}
			ecrireServices(doc)
		}, q.qui(), q.t)
	}
	return flotte.ModifierConfig(ctx, c.db, porteeMachine, func(doc map[string]any) {
		for k, v := range genres {
			doc[k] = v
		}
		ecrireServices(doc)
	}, q.qui(), q.t)
}

// --- Bandeau d'identité -------------------------------------------------------------

// identiteMachine : quelques faits pour le bandeau sous le nom (noyau,
// mémoire, cœurs, durée depuis le démarrage). Vides si la machine n'envoie
// pas de métriques.
type identiteMachine struct {
	Noyau, Memoire, Coeurs, Allumee string
}

func (c *Console) identite(ctx context.Context, m flotte.InfoMachine, ech []alertes.Echantillon, t time.Time) identiteMachine {
	var id identiteMachine
	for _, e := range ech {
		switch e.Etiquettes.Get("__name__") {
		case "node_uname_info":
			id.Noyau = strings.TrimSpace(e.Etiquettes.Get("sysname") + " " + e.Etiquettes.Get("release"))
		case "node_memory_MemTotal_bytes":
			id.Memoire = tailleLisible(int64(e.Valeur))
		case "node_boot_time_seconds":
			if d := t.Sub(time.Unix(int64(e.Valeur), 0)); d > 0 {
				id.Allumee = dureeLisible(d)
			}
		}
	}
	if n, err := c.instant(ctx, fmt.Sprintf(`count(node_cpu_seconds_total{instance=%q,mode="idle"})`, m.Nom), t); err == nil && len(n) == 1 {
		id.Coeurs = pluriel(int(n[0].Valeur), "cœur")
	}
	return id
}
