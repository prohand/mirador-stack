// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/agent/config"
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

	// Détail par élément (SPEC.md § 21) : disques, interfaces, montages et
	// certificats, chacun gardé ou retiré ; chemins de certificats ajoutés.
	Elements     bool // l'agent connaît ces réglages
	SystemeSuivi bool // « Système » est surveillé (sinon pas de détail)
	CertsSuivis  bool // « Certificats » n'est pas retiré
	Composants   []composantSurv
	Certs        []certSurv
	MotifsAjout  []motifSurv
	NbRetires    int // composants et certificats retirés
	// Assistant root plus ancien que l'agent : il n'est mis à jour qu'en
	// relançant le script d'installation (jamais automatiquement).
	VersionRacine, VersionAgent string
	RacineAncienne              bool
}

// motifSurv : un chemin de certificat ajouté et ce qu'il a donné sur la machine.
type motifSurv struct {
	Motif, Resultat, Classe string // Classe : ok, attention ou '' (pas encore lu)
}

// composantSurv : un disque, une interface ou un montage.
type composantSurv struct {
	Genre, Nom, Detail string // genre : disque, interface, montage
	Retire             bool
}

// certSurv : un certificat trouvé (ou retiré) sur la machine.
type certSurv struct {
	Chemin, CN, Emetteur string
	FinS                 int64 // date de fin (secondes Unix), 0 si retiré
	Jours                int   // jours restants (négatif : expiré)
	Retire               bool
}

// metriquesSurv : tout ce qu'il faut lire pour une machine, en une requête.
const metriquesSurv = `mirador_agent_detecte|node_hwmon_temp_celsius|node_thermal_zone_temp|node_hwmon_fan_rpm|` +
	`node_zfs_zpool_state|node_md_degraded|smartctl_device_smart_status|node_firewall_input_policy_drop|` +
	`node_firewall_input_rules|ssl_certs_found_total|node_docker_up|node_docker_container_running|` +
	`node_systemd_enabled_units_total|node_systemd_enabled_units_down|node_systemd_unit_enabled_active|` +
	`apt_upgrades_pending|mirador_logs_journal_lines_read_total|mirador_logs_docker_streams|mirador_agent_info|` +
	`node_uname_info|node_memory_MemTotal_bytes|node_boot_time_seconds|` +
	// Détail par élément (SPEC § 21) : une série par disque, interface,
	// montage et certificat.
	`node_disk_io_time_seconds_total|node_network_receive_bytes_total|node_filesystem_size_bytes|` +
	`ssl_cert_not_after_timestamp_seconds|` +
	// Assistant root (12/11) : sa version, et le compte rendu de chaque
	// chemin de certificat ajouté dans l'interface.
	`mirador_agent_root_info|mirador_agent_certificats_chemin_fichiers|mirador_agent_certificats_chemin_lus|` +
	`mirador_agent_certificats_chemin_ecartes`

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
	certsTotal          *float64
	dockerUp            *float64
	conteneurs          int
	svcTotal, svcDown   *float64
	services            map[string]bool // service → tourne
	majs                *float64
	journal, logsDocker *float64
	agent               bool
	disques, interfaces []string
	montages            map[string]float64 // point de montage → taille (octets)
	certs               []certSurv
	versionAgent        string
	versionRacine       string                        // assistant root ('' = inconnu)
	motifsFichiers      map[string]float64            // chemin ajouté → fichiers trouvés
	motifsLus           map[string]float64            // chemin ajouté → certificats lus
	motifsEcartes       map[string]map[string]float64 // chemin → raison → fichiers écartés
}

func lireReleve(ech []alertes.Echantillon) releve {
	r := releve{detecte: map[string][]string{}, pools: map[string]string{}, grappes: map[string]float64{},
		services: map[string]bool{}, montages: map[string]float64{},
		motifsFichiers: map[string]float64{}, motifsLus: map[string]float64{}, motifsEcartes: map[string]map[string]float64{}}
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
			r.certsTotal = &e.Valeur
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
			r.versionAgent = etq("version")
		case "mirador_agent_root_info":
			r.versionRacine = etq("version")
		case "mirador_agent_certificats_chemin_fichiers":
			r.motifsFichiers[etq("chemin")] = v
		case "mirador_agent_certificats_chemin_lus":
			r.motifsLus[etq("chemin")] = v
		case "mirador_agent_certificats_chemin_ecartes":
			ch := etq("chemin")
			if r.motifsEcartes[ch] == nil {
				r.motifsEcartes[ch] = map[string]float64{}
			}
			r.motifsEcartes[ch][etq("raison")] = v
		case "node_disk_io_time_seconds_total":
			r.disques = append(r.disques, etq("device"))
		case "node_network_receive_bytes_total":
			r.interfaces = append(r.interfaces, etq("device"))
		case "node_filesystem_size_bytes":
			r.montages[etq("mountpoint")] = v
		case "ssl_cert_not_after_timestamp_seconds":
			r.certs = append(r.certs, certSurv{Chemin: etq("path"), CN: etq("cn"), Emetteur: etq("issuer"), FinS: int64(v)})
		}
	}
	for _, l := range r.detecte {
		sort.Strings(l)
	}
	sort.Strings(r.disques)
	sort.Strings(r.interfaces)
	sort.Slice(r.certs, func(i, j int) bool { return r.certs[i].Chemin < r.certs[j].Chemin })
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
		if len(r.disques)+len(r.interfaces)+len(r.montages) == 0 {
			return r.agent, "Processeur, mémoire, disques, réseau", false
		}
		return r.agent, fmt.Sprintf("Processeur, mémoire, %s, %s, %s", pluriel(len(r.disques), "disque"),
			pluriel(len(r.interfaces), "interface"), pluriel(len(r.montages), "montage")), false
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
		if r.certsTotal == nil {
			return false, "", false
		}
		return true, pluriel(int(*r.certsTotal), "certificat") + " suivi(s)", false
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
		// Un service déjà choisi (surveillé ou ignoré) n'est plus « à valider »,
		// même si l'agent le signale encore : il n'applique sa nouvelle
		// configuration qu'à sa prochaine relecture (5 min au plus).
		if cfg.SystemdIgnorer != nil {
			for _, n := range *cfg.SystemdIgnorer {
				vus[n] = true
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
	completerElements(&v, cfg, &r)
	v.NbAValider = len(v.AValider) + len(v.SvcAValider)
	v.NbSurveilles = len(v.Surveilles)
	v.NbIgnores = len(v.Ignores) + len(v.SvcIgnores) + v.NbRetires
	return v
}

// completerElements : détail de « Système » et de « Certificats » (SPEC.md
// § 21). Ce qui est retiré n'envoie plus rien : il vient de la configuration.
func completerElements(v *vueSurveillance, cfg *distant.ConfigDistante, r *releve) {
	v.SystemeSuivi = cfg.Metriques == nil || *cfg.Metriques
	v.CertsSuivis = cfg.EnvoyerCertificats == nil || *cfg.EnvoyerCertificats
	liste := func(p *[]string) []string {
		if p == nil {
			return nil
		}
		return *p
	}
	// Un élément retiré n'apparaît qu'une fois, en « Retiré », même si
	// l'agent l'envoie encore (il n'applique le retrait qu'à sa relecture de
	// la configuration, et sa dernière mesure reste lue quelques minutes).
	ajouter := func(genre string, vus []string, detail func(string) string, retires []string) {
		for _, n := range vus {
			if slices.Contains(retires, n) {
				continue
			}
			v.Composants = append(v.Composants, composantSurv{Genre: genre, Nom: n, Detail: detail(n)})
		}
		for _, n := range retires {
			v.Composants = append(v.Composants, composantSurv{Genre: genre, Nom: n, Retire: true})
			v.NbRetires++
		}
	}
	rien := func(string) string { return "" }
	ajouter("disque", r.disques, rien, liste(cfg.IgnorerDisques))
	ajouter("interface", r.interfaces, rien, liste(cfg.IgnorerInterfaces))
	montages := make([]string, 0, len(r.montages))
	for m := range r.montages {
		montages = append(montages, m)
	}
	sort.Strings(montages)
	ajouter("montage", montages, func(m string) string { return tailleLisible(int64(r.montages[m])) }, liste(cfg.IgnorerMontages))

	retires := liste(cfg.CertificatsIgnorer)
	connus := map[string]certSurv{}
	for _, c := range r.certs {
		if slices.Contains(retires, c.Chemin) {
			connus[c.Chemin] = c // encore envoyé : nom et émetteur gardés pour la ligne « Retiré »
			continue
		}
		v.Certs = append(v.Certs, c)
	}
	for _, c := range retires {
		l := certSurv{Chemin: c, Retire: true}
		if k, ok := connus[c]; ok {
			l.CN, l.Emetteur = k.CN, k.Emetteur
		}
		v.Certs = append(v.Certs, l)
		v.NbRetires++
	}
	v.VersionAgent, v.VersionRacine = r.versionAgent, r.versionRacine
	v.RacineAncienne = r.versionRacine != "" && r.versionAgent != "" && r.versionRacine != r.versionAgent
	for _, m := range liste(cfg.CertificatsAjout) {
		v.MotifsAjout = append(v.MotifsAjout, resultatMotif(m, r, v.RacineAncienne))
	}
}

// resultatMotif : phrase du compte rendu d'un chemin ajouté.
func resultatMotif(m string, r *releve, racineAncienne bool) motifSurv {
	s := motifSurv{Motif: m}
	fichiers, connu := r.motifsFichiers[m]
	lus := r.motifsLus[m]
	switch {
	case !connu && racineAncienne:
		s.Resultat, s.Classe = "pas lu : assistant root trop ancien (relancer le script d'installation)", "attention"
	case !connu:
		s.Resultat = "pas encore lu (l'assistant root passe toutes les 15 min)"
	case fichiers == 0:
		s.Resultat, s.Classe = "aucun fichier trouvé à ce chemin", "attention"
	case lus == 0:
		s.Resultat, s.Classe = pluriel(int(fichiers), "fichier")+" trouvé(s), aucun lu : ", "attention"
		if r := raisonsEcartes(r.motifsEcartes[m]); r != "" {
			s.Resultat += r
		} else { // agent d'avant le 12/11 : raison inconnue
			s.Resultat += "clé privée, lien hors dossier autorisé ou autre format"
		}
	default:
		s.Resultat, s.Classe = pluriel(int(lus), "certificat")+" lu(s)", "ok"
		if e := int(fichiers - lus); e > 0 {
			s.Resultat += fmt.Sprintf(", %d fichier(s) écarté(s)", e)
			if r := raisonsEcartes(r.motifsEcartes[m]); r != "" {
				s.Resultat += " : " + r
			}
		}
	}
	return s
}

// raisonsEcartes : « lien vers un dossier non autorisé (1), clé privée (2) ».
func raisonsEcartes(raisons map[string]float64) string {
	libelles := []struct{ cle, texte string }{
		{"hors_dossier", "lien vers un dossier non autorisé"},
		{"cle", "nom de clé privée"},
		{"illisible", "pas un certificat lisible"},
		{"double", "déjà lu par un autre chemin"},
	}
	var out []string
	for _, l := range libelles {
		if n := int(raisons[l.cle]); n > 0 {
			out = append(out, fmt.Sprintf("%s (%d)", l.texte, n))
		}
	}
	return strings.Join(out, ", ")
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
	v.Elements = m.Elements()
	for i := range v.Certs {
		if c := &v.Certs[i]; !c.Retire {
			c.Jours = int(time.Unix(c.FinS, 0).Sub(t).Hours() / 24)
		}
	}
	return v, ech
}

// nouveautes : nombre d'éléments « à valider » par machine, pour la vue
// d'ensemble (une requête pour tout le parc).
func (c *Console) nouveautes(ctx context.Context, ms []flotte.InfoMachine, t time.Time) map[string]int {
	// Services : une série par service (étiquette « nom »), pour retirer tout
	// de suite ceux déjà choisis dans la configuration (l'agent ne l'applique
	// qu'à sa prochaine relecture). Autres genres : un compte par genre.
	ech, err := c.instant(ctx, `count by (instance, genre) (mirador_agent_detecte{genre!="service"}) or max by (instance, genre, nom) (mirador_agent_detecte{genre="service"})`, t)
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
		cfg, lu := configs[nom]
		if !lu {
			if id, ok := ids[nom]; ok {
				cfg, _ = flotte.ConfigEffective(ctx, c.db, id)
			}
			configs[nom] = cfg
		}
		if genre == "service" {
			if !serviceChoisi(cfg, e.Etiquettes.Get("nom")) {
				out[nom]++
			}
			continue
		}
		// Ventilateurs, ZFS, RAID : déjà refusé pour cette machine ?
		if cfg == nil || valeurSurv(cfg, genre) == nil {
			out[nom]++
		}
	}
	return out
}

// cacheAValider : nombre d'éléments « à valider » par machine, gardé 60 s.
// Il sert au compteur du menu (toutes les pages) et à la liste des machines :
// sans cache, chaque page relirait la configuration des machines.
type cacheAValider struct {
	mu         sync.Mutex
	expire     time.Time
	parMachine map[string]int
}

// aValider : éléments « à valider » par machine (approuvées seulement).
func (c *Console) aValider(ctx context.Context, t time.Time) map[string]int {
	c.cacheValider.mu.Lock()
	defer c.cacheValider.mu.Unlock()
	if c.cacheValider.parMachine != nil && t.Before(c.cacheValider.expire) {
		return c.cacheValider.parMachine
	}
	out := map[string]int{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		var approuvees []flotte.InfoMachine
		for _, m := range ms {
			if m.Statut == "approuvee" {
				approuvees = append(approuvees, m)
			}
		}
		for nom, n := range c.nouveautes(ctx, approuvees, t) {
			if n > 0 {
				out[nom] = n
			}
		}
	}
	c.cacheValider.parMachine, c.cacheValider.expire = out, t.Add(time.Minute)
	return out
}

// serviceChoisi : service déjà surveillé ou ignoré dans la configuration.
func serviceChoisi(cfg *distant.ConfigDistante, nom string) bool {
	if cfg == nil {
		return false
	}
	for _, l := range []*[]string{cfg.SystemdSurveiller, cfg.SystemdIgnorer} {
		if l != nil && slices.Contains(*l, nom) {
			return true
		}
	}
	return false
}

// oublierAValider : après un choix enregistré, le compteur est recalculé.
func (c *Console) oublierAValider() {
	c.cacheValider.mu.Lock()
	c.cacheValider.parMachine = nil
	c.cacheValider.mu.Unlock()
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
		case "oublier":
			// Service supprimé de la machine (ou choix à refaire) : retiré de
			// la configuration. S'il existe encore, il revient « à valider »
			// (ou est surveillé d'office sans validation un par un).
			delete(surveiller, nom)
			delete(ignorer, nom)
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
	// Disques, interfaces, montages, certificats (SPEC.md § 21) : toujours
	// à la portée de la machine.
	elements, err := choixElements(q, cfg)
	if err != nil {
		return err
	}
	if elements != nil && !m.Elements() {
		return fmt.Errorf("le choix élément par élément demande un agent plus récent sur %s : le mettre à jour d'abord", m.Nom)
	}
	if len(genres) == 0 && !services && elements == nil {
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
		for k, v := range elements {
			doc[k] = v
		}
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

// choixElements lit les choix élément par élément du formulaire :
//
//	e.disque.NOM, e.interface.NOM, e.montage.CHEMIN = retirer | retablir
//	c.CHEMIN (certificat) = retirer | retablir
//	cm.MOTIF (chemin ajouté) = retirer
//	certificats_nouveaux = chemins à ajouter, un par ligne
//
// Renvoie les clés à écrire (nil = liste vide, clé retirée), ou nil si rien
// n'a changé.
func choixElements(q *req, cfg *distant.ConfigDistante) (map[string]any, error) {
	ensemble := func(p *[]string) map[string]bool {
		out := map[string]bool{}
		if p != nil {
			for _, n := range *p {
				out[n] = true
			}
		}
		return out
	}
	listes := map[string]map[string]bool{
		"ignorer_disques":     ensemble(cfg.IgnorerDisques),
		"ignorer_interfaces":  ensemble(cfg.IgnorerInterfaces),
		"ignorer_montages":    ensemble(cfg.IgnorerMontages),
		"certificats_ignorer": ensemble(cfg.CertificatsIgnorer),
		"certificats_ajout":   ensemble(cfg.CertificatsAjout),
	}
	cleDe := map[string]string{"e.disque.": "ignorer_disques", "e.interface.": "ignorer_interfaces",
		"e.montage.": "ignorer_montages", "c.": "certificats_ignorer", "cm.": "certificats_ajout"}
	changees := map[string]bool{}
	n := 0
	for champ, valeurs := range q.r.PostForm {
		if len(valeurs) == 0 || valeurs[0] == "" {
			continue
		}
		for prefixe, cle := range cleDe {
			nom, ok := strings.CutPrefix(champ, prefixe)
			if !ok {
				continue
			}
			if n++; n > 600 {
				return nil, errors.New("trop d'éléments dans la demande")
			}
			switch {
			case valeurs[0] == "retirer" && cle == "certificats_ajout":
				delete(listes[cle], nom)
			case valeurs[0] == "retirer":
				listes[cle][nom] = true
			case valeurs[0] == "retablir" && cle != "certificats_ajout":
				delete(listes[cle], nom)
			default:
				return nil, errors.New("choix inconnu")
			}
			changees[cle] = true
		}
	}
	for _, ligne := range strings.Split(q.r.PostFormValue("certificats_nouveaux"), "\n") {
		ligne = strings.TrimSpace(ligne)
		if ligne == "" {
			continue
		}
		if err := config.ValiderMotifDistant(ligne); err != nil {
			return nil, fmt.Errorf("chemin de certificat refusé : %v", err)
		}
		listes["certificats_ajout"][ligne] = true
		changees["certificats_ajout"] = true
	}
	if len(changees) == 0 {
		return nil, nil
	}
	if len(listes["certificats_ajout"]) > config.MaxMotifsDistants {
		return nil, fmt.Errorf("%d chemins de certificats ajoutés au plus", config.MaxMotifsDistants)
	}
	out := map[string]any{}
	for cle := range changees {
		if len(listes[cle]) == 0 {
			out[cle] = nil // clé retirée
			continue
		}
		l := make([]string, 0, len(listes[cle]))
		for n := range listes[cle] {
			l = append(l, n)
		}
		sort.Strings(l)
		out[cle] = l
	}
	return out, nil
}

// --- Bandeau d'identité -------------------------------------------------------------

// identiteMachine : quelques faits pour le bandeau sous le nom (noyau,
// mémoire, cœurs, durée depuis le démarrage). Vides si la machine n'envoie
// pas de métriques.
type identiteMachine struct {
	Noyau, Memoire, Coeurs, Allumee string
	// Intervalle d'envoi des métriques (réglage effectif, défaut 30 s) et
	// lien vers son réglage pour cette machine.
	Intervalle, LienIntervalle string
	// Mises à jour de paquets (apt) : -1 = pas de valeur.
	Paquets, PaquetsSecurite int
	Redemarrage              bool
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
	id.Intervalle = "30 s"
	if cfg, err := flotte.ConfigEffective(ctx, c.db, m.ID); err == nil && cfg != nil && cfg.Intervalle != nil {
		id.Intervalle = dureeLisible(time.Duration(*cfg.Intervalle))
	}
	id.LienIntervalle = fmt.Sprintf("/reglages/agents?portee=machine:%d", m.ID)
	// Mises à jour de paquets : dernière valeur des 3 dernières heures (l'agent
	// les compte toutes les quelques heures seulement).
	id.Paquets = -1
	sel := fmt.Sprintf(`{__name__=~"apt_upgrades_pending|apt_upgrades_security_pending|node_reboot_required", instance=%q}`, m.Nom)
	if ech, err := c.instant(ctx, `last_over_time(`+sel+`[3h])`, t); err == nil {
		for _, e := range ech {
			switch e.Etiquettes.Get("__name__") {
			case "apt_upgrades_pending":
				id.Paquets = int(e.Valeur)
			case "apt_upgrades_security_pending":
				id.PaquetsSecurite = int(e.Valeur)
			case "node_reboot_required":
				id.Redemarrage = e.Valeur > 0
			}
		}
	}
	return id
}
