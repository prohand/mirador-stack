// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/fluxbloques"
)

// Bloc « Pare-feu » de la page d'une machine (SPEC § 136, maquette validée
// le 27/11) : ce que l'assistant root a trouvé, ce que mirador reçoit, le
// réglage de lecture et, s'il le faut, la règle à ajouter (texte à copier :
// mirador ne modifie jamais le pare-feu).

type commandePareFeu struct {
	Outil, Texte, Note string
	Detecte            bool
}

type vuePareFeu struct {
	Nom          string
	Equipement   bool   // équipement d'un collecteur (OPNsense par filterlog)
	Collecteur   string // son collecteur
	Outil        string // iptables, nftables, aucun ; vide : inconnu
	Politique    string
	Regles       int
	Gestion      string // ufw, firewalld
	JournalConnu bool   // assistant root assez récent pour le dire
	Journal      int    // règles de journalisation
	SansLimite   int
	Prefixes     []string
	Recus24h     int // entrants + sortants comptés sur 24 h
	Dernier      time.Time
	NbIP, NbPays int
	Filterlog1h  int // équipement : lignes filterlog reçues par le collecteur (1 h)
	GarderFL     bool
	R            fluxbloques.Reglage
	PrefixesTxt  string
	Etat, Classe string
	// Conseil : « ajouter » (aucune règle de journalisation) ou « limite »
	// (règles sans limite de débit) ; commandes à copier, outil détecté d'abord.
	Conseil   string
	Commandes []commandePareFeu
}

// pareFeuDe : le bloc d'une machine à agent ou d'un équipement de collecteur.
func (c *Console) pareFeuDe(ctx context.Context, m flotte.InfoMachine, relevePar string, t time.Time) *vuePareFeu {
	if c.opt.Flux == nil || m.Type == "collecteur" {
		return nil
	}
	v := &vuePareFeu{Nom: m.Nom, Equipement: relevePar != "", Collecteur: relevePar, R: c.reglagePareFeu(m.Nom)}
	v.PrefixesTxt = strings.Join(v.R.Prefixes, ", ")
	inst := `instance=` + strconv.Quote(m.Nom)
	// Ce que l'assistant root a lu (dernières valeurs).
	if e, err := c.instant(ctx, `last_over_time(node_firewall_input_rules{`+inst+`}[1h])`, t); err == nil && len(e) > 0 {
		v.Outil, v.Politique, v.Regles = e[0].Etiquettes.Get("tool"), e[0].Etiquettes.Get("policy"), int(e[0].Valeur)
	}
	if e, err := c.instant(ctx, `last_over_time(node_firewall_log_rules{`+inst+`}[1h])`, t); err == nil && len(e) > 0 {
		v.JournalConnu, v.Journal = true, int(e[0].Valeur)
	}
	if e, err := c.instant(ctx, `last_over_time(node_firewall_log_rules_unlimited{`+inst+`}[1h])`, t); err == nil && len(e) > 0 {
		v.SansLimite = int(e[0].Valeur)
	}
	if e, err := c.instant(ctx, `last_over_time(node_firewall_log_prefix{`+inst+`}[1h])`, t); err == nil {
		for _, x := range e {
			v.Prefixes = append(v.Prefixes, x.Etiquettes.Get("prefix"))
		}
		sort.Strings(v.Prefixes)
	}
	if e, err := c.instant(ctx, `last_over_time(node_firewall_manager{`+inst+`}[1h])`, t); err == nil && len(e) > 0 {
		v.Gestion = e[0].Etiquettes.Get("name")
	}
	// Ce que mirador reçoit (24 h, les deux sens).
	for _, mag := range []*fluxbloques.Magasin{c.opt.Flux.Entrants, c.opt.Flux.Sortants} {
		if b, err := mag.Bilan(ctx, t.Add(-24*time.Hour), t, fluxbloques.Filtre{PareFeu: m.Nom}, 24); err == nil {
			v.Recus24h += b.Total
			if mag == c.opt.Flux.Entrants {
				v.NbIP, v.NbPays = b.NbIP, b.NbPays
			}
		}
		if d := mag.DernierDe(m.Nom); d.After(v.Dernier) {
			v.Dernier = d
		}
	}
	if v.Equipement {
		if r := c.parEtiquette(ctx, `sum(increase(mirador_collecteur_syslog_refus_total{instance=`+strconv.Quote(relevePar)+`, motif="filterlog"}[1h]))`, t); len(r) > 0 {
			for _, n := range r {
				v.Filterlog1h = int(n)
			}
		}
		if col, err := c.syslogCollecteurDe(ctx, relevePar); err == nil {
			v.GarderFL = col
		}
	}
	// État et conseil.
	switch {
	case v.R.Lecture == fluxbloques.LectureNon:
		v.Etat, v.Classe = "lecture désactivée", ""
	case v.Recus24h > 0:
		v.Etat, v.Classe = "blocages reçus", "ok"
	default:
		v.Etat, v.Classe = "rien reçu", "attention"
	}
	if !v.Equipement && v.R.Lecture != fluxbloques.LectureNon && v.JournalConnu {
		switch {
		case v.Journal == 0 && v.Recus24h == 0: // des blocages arrivent : une règle existe ailleurs
			v.Conseil = "ajouter"
		case v.SansLimite > 0:
			v.Conseil = "limite"
		}
	}
	if v.Conseil != "" {
		v.Commandes = commandesPareFeu(v.Outil, v.Gestion, v.Conseil, v.R.Sortants)
	}
	return v
}

// syslogCollecteurDe : « Garder filterlog » du collecteur (par son nom).
func (c *Console) syslogCollecteurDe(ctx context.Context, collecteur string) (bool, error) {
	var id int64
	if err := c.db.QueryRowContext(ctx, `SELECT id FROM machines WHERE nom = ?`, collecteur).Scan(&id); err != nil {
		return false, err
	}
	cfg, err := flotte.ConfigEffective(ctx, c.db, id)
	if err != nil || cfg.Collecteur == nil || cfg.Collecteur.Syslog == nil {
		return false, err
	}
	return cfg.Collecteur.Syslog.GarderFilterlog, nil
}

// commandesPareFeu : règles à copier, avec limite de débit (10 par seconde,
// rafale de 20). L'outil détecté vient en premier.
func commandesPareFeu(outil, gestion, conseil string, sortants bool) []commandePareFeu {
	var cs []commandePareFeu
	ipt := `iptables -A INPUT -m limit --limit 10/s --limit-burst 20 -j LOG --log-prefix "DROP_IN: "`
	iptNote := "À placer juste avant la règle qui bloque (avec une politique DROP, en fin de chaîne : -A suffit). À garder : netfilter-persistent save."
	if conseil == "limite" {
		ipt = "iptables -L INPUT --line-numbers   # repérer le n° de la règle LOG\n" +
			`iptables -R INPUT <n°> -m limit --limit 10/s --limit-burst 20 -j LOG --log-prefix "DROP_IN: "`
		iptNote = "Remplace la règle LOG sans limite par la même, limitée. À garder : netfilter-persistent save."
	}
	if sortants {
		ipt += "\n" + `iptables -A OUTPUT -m limit --limit 10/s --limit-burst 20 -j LOG --log-prefix "DROP_OUT: "`
	}
	nft := `nft add rule inet filter input limit rate 10/second burst 20 packets log prefix "DROP_IN: "`
	if sortants {
		nft += "\n" + `nft add rule inet filter output limit rate 10/second burst 20 packets log prefix "DROP_OUT: "`
	}
	cs = append(cs,
		commandePareFeu{Outil: "ufw", Texte: "ufw logging low", Note: "ufw limite déjà le débit de sa journalisation.", Detecte: gestion == "ufw"},
		commandePareFeu{Outil: "firewalld", Texte: "firewall-cmd --set-log-denied=unicast", Note: "Réglage gardé par firewalld.", Detecte: gestion == "firewalld"},
		commandePareFeu{Outil: "nftables", Texte: nft, Note: "Nom de la table et des chaînes à adapter ; à recopier dans /etc/nftables.conf pour le redémarrage.",
			Detecte: gestion == "" && outil == "nftables"},
		commandePareFeu{Outil: "iptables", Texte: ipt, Note: iptNote, Detecte: gestion == "" && outil != "nftables"},
	)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Detecte && !cs[j].Detecte })
	return cs
}

// ligneSource : une ligne du tableau « Sources » de la page Flux bloqués.
type ligneSource struct {
	Machine, PareFeu, Journal, Classe, Detail string
	Prefixes                                  []string
	Bloques                                   int
	Dernier                                   time.Time
	Lien                                      string
}

// tableauSources : toutes les machines dont le pare-feu est relevé ou qui
// ont envoyé des blocages (24 h, les deux sens), les plus actives d'abord.
func (c *Console) tableauSources(ctx context.Context, t time.Time) []ligneSource {
	if c.opt.Flux == nil {
		return nil
	}
	lignes := map[string]*ligneSource{}
	ligne := func(m string) *ligneSource {
		if l, ok := lignes[m]; ok {
			return l
		}
		l := &ligneSource{Machine: m, Lien: "/machines/" + m + "?onglet=reglages#parefeu"}
		lignes[m] = l
		return l
	}
	outils := map[string]string{}
	for k := range c.parEtiquette(ctx, `max by (instance, tool) (last_over_time(node_firewall_input_rules[1h]))`, t, "instance", "tool") {
		if m, outil, ok := strings.Cut(k, "\x00"); ok && m != "" {
			ligne(m)
			if outil != "" && outil != "aucun" {
				outils[m] = outil
			} else if _, deja := outils[m]; !deja {
				outils[m] = outil
			}
		}
	}
	gestion := c.parEtiquette(ctx, `max by (instance, name) (last_over_time(node_firewall_manager[1h]))`, t, "instance", "name")
	for k := range gestion {
		if m, nom, ok := strings.Cut(k, "\x00"); ok {
			outils[m] = nom
		}
	}
	regles := c.parEtiquette(ctx, `max by (instance) (last_over_time(node_firewall_log_rules[1h]))`, t, "instance")
	sansLimite := c.parEtiquette(ctx, `max by (instance) (last_over_time(node_firewall_log_rules_unlimited[1h]))`, t, "instance")
	prefixes := map[string][]string{}
	for k := range c.parEtiquette(ctx, `max by (instance, prefix) (last_over_time(node_firewall_log_prefix[1h]))`, t, "instance", "prefix") {
		if m, p, ok := strings.Cut(k, "\x00"); ok {
			prefixes[m] = append(prefixes[m], p)
		}
	}
	for _, mag := range []*fluxbloques.Magasin{c.opt.Flux.Entrants, c.opt.Flux.Sortants} {
		if b, err := mag.Bilan(ctx, t.Add(-24*time.Hour), t, fluxbloques.Filtre{}, 24); err == nil {
			for _, x := range b.PareFeux {
				if x.Nom != fluxbloques.Autres {
					ligne(x.Nom).Bloques += x.N
				}
			}
		}
	}
	out := make([]ligneSource, 0, len(lignes))
	for m, l := range lignes {
		for _, mag := range []*fluxbloques.Magasin{c.opt.Flux.Entrants, c.opt.Flux.Sortants} {
			if d := mag.DernierDe(m); d.After(l.Dernier) {
				l.Dernier = d
			}
		}
		sort.Strings(prefixes[m])
		l.Prefixes = prefixes[m]
		l.PareFeu = outils[m]
		r := c.reglagePareFeu(m)
		n, connu := regles[m]
		switch {
		case r.Lecture == fluxbloques.LectureNon:
			l.Journal, l.Classe = "lecture désactivée", ""
		case !connu && outils[m] == "" && l.Bloques > 0:
			l.PareFeu, l.Journal, l.Classe = "équipement (collecteur)", "reçues", "ok"
		case !connu:
			l.Journal, l.Classe, l.Detail = "inconnue", "", "assistant root à mettre à jour"
		case n == 0 && l.Bloques > 0:
			l.Journal, l.Classe, l.Detail = "reçues", "ok", "règle non vue par l'assistant root"
		case n == 0:
			l.Journal, l.Classe, l.Detail = "aucune règle log", "erreur", "règle à ajouter"
		case sansLimite[m] > 0:
			l.Journal, l.Classe, l.Detail = "oui, sans limite", "attention", "limite conseillée"
		default:
			l.Journal, l.Classe = "oui, limitée", "ok"
		}
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bloques != out[j].Bloques {
			return out[i].Bloques > out[j].Bloques
		}
		return out[i].Machine < out[j].Machine
	})
	return out
}
