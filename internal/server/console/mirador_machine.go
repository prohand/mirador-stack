// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Agent et assistant root, plus clairs (SPEC § 124, maquette validée le
// 23/11) : les deux morceaux de mirador sur une machine, côte à côte, avec
// ce que chacun peut et ne peut pas faire. Même chose pour un collecteur
// (collecteur et compagnon des processus, aucun assistant root).

// periodeRacine : l'assistant root est lancé par un timer toutes les 15 min.
const periodeRacine = 15 * time.Minute

// ageRacineAncien : 3 tours manqués.
const ageRacineAncien = 45 * time.Minute

// etatRacine : assistant root d'une machine, d'après les séries de l'agent.
type etatRacine struct {
	Version     string
	Age         time.Duration // âge du dernier relevé
	Erreurs     int
	AJour       bool
	ConnuAJour  bool // série « à jour » reçue (agent récent)
	PareFeu     bool
	Disques     int // SMART
	Certificats int
}

// Lit : ce que l'assistant a relevé (« pare-feu, SMART, certificats »).
func (r etatRacine) Lit() []string {
	var out []string
	if r.PareFeu {
		out = append(out, "pare-feu")
	}
	if r.Disques > 0 {
		out = append(out, "SMART")
	}
	if r.Certificats > 0 {
		out = append(out, "certificats")
	}
	return out
}

// Ancien : relevé trop vieux (assistant arrêté ou timer coupé).
func (r etatRacine) Ancien() bool { return r.Age > ageRacineAncien }

// racines : assistant root de chaque machine (clé : instance). Machines
// sans assistant absentes. 6 requêtes instantanées, en cache 20 s.
func (c *Console) racines(ctx context.Context, t time.Time) map[string]etatRacine {
	out := map[string]etatRacine{}
	ages := c.parEtiquette(ctx, `max by (instance) (mirador_agent_root_age_seconds)`, t, "instance")
	if len(ages) == 0 {
		return out
	}
	erreurs := c.parEtiquette(ctx, `max by (instance) (mirador_agent_root_errors)`, t, "instance")
	ajour := c.parEtiquette(ctx, `max by (instance) (mirador_agent_root_a_jour)`, t, "instance")
	parefeu := c.parEtiquette(ctx, `count by (instance) (node_firewall_input_rules)`, t, "instance")
	disques := c.parEtiquette(ctx, `count by (instance) (smartctl_device_smart_status)`, t, "instance")
	certs := c.parEtiquette(ctx, `max by (instance) (ssl_certs_found_total)`, t, "instance")
	versions := map[string]string{}
	if ech, err := c.instant(ctx, `max by (instance, version) (mirador_agent_root_info)`, t); err == nil {
		for _, e := range ech {
			versions[e.Etiquettes.Get("instance")] = e.Etiquettes.Get("version")
		}
	}
	for inst, a := range ages {
		r := etatRacine{Version: versions[inst], Age: time.Duration(a) * time.Second, Erreurs: int(erreurs[inst]),
			PareFeu: parefeu[inst] > 0, Disques: int(disques[inst]), Certificats: int(certs[inst])}
		if v, ok := ajour[inst]; ok {
			r.ConnuAJour, r.AJour = true, v == 1
		}
		out[inst] = r
	}
	return out
}

// --- Page machine : bloc « mirador sur cette machine » (onglet Réglages) -----------

type defMorceau struct {
	Titre, Texte, Classe string
	Lien, LienTexte      string
	Puces                []puceMorceau
}

type puceMorceau struct{ Texte, Classe string }

// morceauMirador : un des deux morceaux (agent, assistant root, collecteur,
// compagnon).
type morceauMirador struct {
	Nom, Version     string
	Pastille, Classe string // « sans root » (ok), « root · sans réseau » (attention)
	Icone, Teinte    string
	Root             bool // encadré orange
	Absent           string
	Lignes           []defMorceau
}

type blocMirador struct {
	Premier, Second morceauMirador // agent (ou collecteur), assistant root (ou compagnon)
	Pourquoi        string         // phrase « pourquoi deux morceaux »
	Fleche          string
	Actualiser      bool
	Collecteur      bool
}

// blocMiradorMachine : agent (ou collecteur) et son second morceau.
func (c *Console) blocMiradorMachine(ctx context.Context, m flotte.InfoMachine, maj vueMaj, t time.Time) *blocMirador {
	if m.Version == "" {
		return nil // envoi direct (Telegraf…) ou équipement : aucun morceau de mirador
	}
	dernier := "jamais"
	if m.VuLe > 0 {
		dernier = "il y a " + dureeLisible(t.Sub(time.Unix(m.VuLe, 0)))
	}
	ligneMaj := defMorceau{Titre: "Mise à jour", Texte: "à jour · version signée", Classe: "ok"}
	switch {
	case maj.Echec != "":
		ligneMaj = defMorceau{Titre: "Mise à jour", Texte: premierTexte(maj.Cible, "dernière version") + " en échec", Classe: "erreur", Lien: "#maj", LienTexte: "voir"}
	case maj.EnAttente():
		ligneMaj = defMorceau{Titre: "Mise à jour", Texte: maj.Cible + " en attente", Classe: "attention"}
	}

	if m.Type == "collecteur" {
		b := &blocMirador{Collecteur: true, Fleche: "lit la liste des processus",
			Premier: morceauMirador{Nom: "mirador-collector", Version: versionCourte(m.Version), Pastille: "sans root", Classe: "ok", Icone: "reseau", Teinte: "vert",
				Lignes: []defMorceau{
					{Titre: "Utilisateur", Texte: "mirador-collector (pas root)"},
					{Titre: "Réseau", Texte: "envoie au serveur ; écoute seulement sur le LAN"},
					{Titre: "Dernier envoi", Texte: dernier},
					ligneMaj,
				}},
			Second: morceauMirador{Nom: "Compagnon des processus", Pastille: "sans root · sans réseau", Classe: "ok", Icone: "cpu", Teinte: "vert",
				Lignes: []defMorceau{
					{Titre: "Utilisateur", Texte: "mirador-processus (pas root)"},
					{Titre: "Rôle", Texte: "lit la liste des processus pour « En ce moment »"},
				}},
			Pourquoi: "Pas d'assistant root sur un collecteur : rien n'est lu en root sur cette machine (seul le réglage du ping se fait en root, au démarrage). Pas de SMART, de pare-feu ni de certificats protégés ici."}
		inst := `instance="` + echapperPromQL(m.Nom) + `"`
		v, ok := c.une(ctx, `max(last_over_time(mirador_collecteur_compagnon{`+inst+`}[10m]))`, t)
		switch {
		case !ok:
			b.Second.Absent = "arrive avec le collecteur 0.6.13 (puis relancer une fois le script d'installation)"
		case v == 0:
			b.Second.Absent = "pas installé : relancer une fois le script d'installation du collecteur"
		default:
			b.Second.Lignes = append(b.Second.Lignes, defMorceau{Titre: "État", Texte: "en marche", Classe: "ok"})
		}
		return b
	}

	b := &blocMirador{Fleche: "lit le résultat dans un fichier",
		Premier: morceauMirador{Nom: "mirador-agent", Version: versionCourte(m.Version), Pastille: "sans root", Classe: "ok", Icone: "machines", Teinte: "vert",
			Lignes: []defMorceau{
				{Titre: "Utilisateur", Texte: "mirador-agent (pas root)"},
				{Titre: "Réseau", Texte: "vers le serveur seulement, aucun port ouvert"},
				{Titre: "Dernier envoi", Texte: dernier},
				ligneMaj,
			}},
		Second: morceauMirador{Nom: "Assistant root", Pastille: "root · sans réseau", Classe: "attention", Icone: "bouclier", Teinte: "orange", Root: true},
		Pourquoi: "Pourquoi deux morceaux : l'agent parle au serveur et se met à jour seul, il ne doit donc pas être root. " +
			"Ce qui exige root (pare-feu, SMART, certificats protégés) est lu par l'assistant, qui n'a pas de réseau et ne lance que des commandes figées.",
		Actualiser: true}
	// Agent dans un conteneur (LXC, Docker…) : le matériel vu là est celui de
	// l'hôte, l'agent ne le lit pas (SPEC § 125).
	if ech, err := c.instant(ctx, `max by (genre) (mirador_agent_conteneur{instance="`+echapperPromQL(m.Nom)+`"})`, t); err == nil && len(ech) > 0 {
		g := ech[0].Etiquettes.Get("genre")
		if g == "lxc" || g == "docker" {
			g = strings.ToUpper(g[:1]) + g[1:]
			if g == "Lxc" {
				g = "LXC"
			}
		}
		b.Premier.Lignes = append(b.Premier.Lignes, defMorceau{Titre: "Conteneur",
			Texte: g + " : capteurs, ventilateurs, ZFS et RAID de l'hôte ignorés (surveillés par l'agent de l'hôte)"})
	}
	r, ok := c.racines(ctx, t)[m.Nom]
	if !ok {
		b.Second.Absent = "pas d'assistant root : rien n'est lu en root sur cette machine (pas de SMART, de pare-feu ni de certificats protégés)"
		return b
	}
	b.Second.Version = versionCourte(r.Version)
	b.Second.Lignes = append(b.Second.Lignes, defMorceau{Titre: "Quand", Texte: "toutes les 15 min, quelques secondes, puis s'arrête"})
	rel := defMorceau{Titre: "Dernier relevé", Texte: "il y a " + dureeLisible(r.Age)}
	switch {
	case r.Ancien():
		rel.Classe, rel.Texte = "erreur", "il y a "+dureeLisible(r.Age)+" : l'assistant ne tourne plus (timer mirador-agent-root)"
	case r.Age < periodeRacine:
		rel.Texte += " · prochain dans " + dureeLisible((periodeRacine - r.Age).Round(time.Minute))
	}
	b.Second.Lignes = append(b.Second.Lignes, rel)
	lit := defMorceau{Titre: "Lit"}
	if r.PareFeu {
		lit.Puces = append(lit.Puces, puceMorceau{"pare-feu", "ok"})
	}
	if r.Disques > 0 {
		lit.Puces = append(lit.Puces, puceMorceau{"SMART · " + strconv.Itoa(r.Disques) + " " + accorder(r.Disques, "disque(s)"), "ok"})
	}
	if r.Certificats > 0 {
		lit.Puces = append(lit.Puces, puceMorceau{"certificats · " + strconv.Itoa(r.Certificats), "ok"})
	}
	if len(lit.Puces) == 0 {
		lit.Texte = "rien pour l'instant"
	}
	b.Second.Lignes = append(b.Second.Lignes, lit)
	e := defMorceau{Titre: "Erreurs", Texte: "aucune"}
	if r.Erreurs > 0 {
		e.Texte, e.Classe = strconv.Itoa(r.Erreurs)+" (voir son journal : journalctl -u mirador-agent-root)", "attention"
	}
	b.Second.Lignes = append(b.Second.Lignes, e)
	if r.ConnuAJour && !r.AJour {
		b.Second.Lignes = append(b.Second.Lignes, defMorceau{Titre: "Version", Texte: "à mettre à jour (se fait avec la prochaine mise à jour de l'agent)", Classe: "attention"})
	}
	return b
}

// plusVieux : âge le plus grand (relevés des assistants root).
func plusVieux(rs map[string]etatRacine) time.Duration {
	var v float64
	for _, r := range rs {
		v = math.Max(v, float64(r.Age))
	}
	return time.Duration(v)
}

// --- Santé de mirador : tableau « Par machine » ----------------------------------

// ligneParMachine : agent et assistant root d'une machine.
type ligneParMachine struct {
	Machine              string
	Agent, ClasseAgent   string // « 0.6.15 », « 0.6.14 · échec », « muette »
	Racine, ClasseRacine string // « 0.6.15 », "" (non installé)
	Releve, ClasseReleve string // « il y a 4 min · pare-feu, SMART »
}

// lignesParMachine : une ligne par machine avec agent, par nom.
func lignesParMachine(avecAgent []flotte.InfoMachine, rs map[string]etatRacine, cible string, t time.Time) []ligneParMachine {
	var out []ligneParMachine
	for _, m := range avecAgent {
		l := ligneParMachine{Machine: m.Nom, Agent: versionCourte(m.Version), ClasseAgent: "ok"}
		cm := cible
		if m.VersionCible != "" {
			cm = m.VersionCible
		}
		switch etatMaj(m, cm) {
		case majEchec:
			l.Agent, l.ClasseAgent = l.Agent+" · échec", "erreur"
		case majAJour:
		default:
			l.ClasseAgent = "attention"
		}
		if t.Sub(time.Unix(m.VuLe, 0)) > delaiMuette {
			l.Agent, l.ClasseAgent = "muette", ""
		}
		r, ok := rs[m.Nom]
		switch {
		case !ok:
			l.Releve = "non installé : rien à lire en root sur cette machine"
		default:
			l.Racine, l.ClasseRacine = premierTexte(versionCourte(r.Version), "présent"), "ok"
			if r.ConnuAJour && !r.AJour {
				l.ClasseRacine = "attention"
			}
			l.Releve = "il y a " + dureeLisible(r.Age)
			if lit := r.Lit(); len(lit) > 0 {
				l.Releve += " · " + joindreVirgules(lit)
			}
			switch {
			case r.Ancien():
				l.Releve, l.ClasseReleve = "pas de relevé depuis "+dureeLisible(r.Age), "erreur"
			case r.Erreurs > 0:
				l.Releve += " · " + strconv.Itoa(r.Erreurs) + " " + accorder(r.Erreurs, "erreur(s)")
				l.ClasseReleve = "attention"
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out
}

func joindreVirgules(s []string) string { return strings.Join(s, ", ") }
