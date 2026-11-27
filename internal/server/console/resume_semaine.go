// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/fluxbloques"
	"mirador-stack/internal/server/notif"
)

// Résumé de la semaine (demande du 27/11, maquette validée) : un message
// d'information, une fois par semaine, sur les canaux de l'info. Une ligne
// n'apparaît que s'il y a quelque chose à dire. Désactivable dans
// Réglages › Notifications. Réglage gardé à part des réglages des
// notifications (clé propre) : un retour à une version d'avant ne les abîme pas.

const (
	cleResume        = "resume.semaine" // réglage (JSON)
	cleResumeDernier = "resume.dernier" // jour du dernier envoi (AAAA-MM-JJ, fuseau de l'interface)
)

// ReglageResume : jour (1 = lundi … 7 = dimanche) et heure d'envoi.
type ReglageResume struct {
	Actif bool   `json:"actif"`
	Jour  int    `json:"jour"`
	Heure string `json:"heure"` // « 08:00 »
}

// Jours : noms affichés (index 1 à 7).
var joursResume = []string{"", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi", "dimanche"}

func resumeDefaut() ReglageResume { return ReglageResume{Actif: true, Jour: 1, Heure: "08:00"} }

func (r ReglageResume) valider() error {
	if r.Jour < 1 || r.Jour > 7 {
		return errors.New("jour du résumé : lundi à dimanche")
	}
	if _, _, ok := heureMinute(r.Heure); !ok {
		return errors.New("heure du résumé : HH:MM")
	}
	return nil
}

// NomJour : « lundi ».
func (r ReglageResume) NomJour() string { return joursResume[min(max(r.Jour, 1), 7)] }

// heureMinute : « 08:00 » → 8, 0.
func heureMinute(s string) (int, int, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

func (c *Console) lireResume(ctx context.Context) ReglageResume {
	r := resumeDefaut()
	if ok, err := lireJSONReglage(ctx, c.db, cleResume, &r); ok && (err != nil || r.valider() != nil) {
		return resumeDefaut()
	}
	return r
}

func (c *Console) ecrireResume(ctx context.Context, r ReglageResume, qui, ip string, t time.Time) error {
	if err := r.valider(); err != nil {
		return err
	}
	avant := c.lireResume(ctx)
	brut, _ := json.Marshal(r)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleResume, string(brut), t.Unix(), qui); err != nil {
		return err
	}
	brutAvant, _ := json.Marshal(avant)
	if err := Auditer(ctx, tx, qui, "resume_semaine", "", string(brutAvant), string(brut), ip, t); err != nil {
		return err
	}
	return tx.Commit()
}

// tourResume : appelé chaque minute ; envoie le résumé le jour réglé, à
// partir de l'heure réglée, une seule fois ce jour-là (un serveur redémarré
// à 15 h l'envoie à 15 h, pas le lendemain).
func (c *Console) tourResume(ctx context.Context, t time.Time) {
	if c.informer == nil {
		return
	}
	r := c.lireResume(ctx)
	l := t.In(fuseau.Load())
	jour := int(l.Weekday())
	if jour == 0 {
		jour = 7
	}
	h, m, _ := heureMinute(r.Heure)
	if !r.Actif || jour != r.Jour || l.Hour()*60+l.Minute() < h*60+m {
		return
	}
	aujourdhui := l.Format("2006-01-02")
	var dernier string
	_ = c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleResumeDernier).Scan(&dernier)
	if dernier == aujourdhui {
		return
	}
	// Noté avant l'envoi : un envoi raté n'est pas répété toutes les minutes.
	if _, err := c.db.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, 'mirador')
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le`, cleResumeDernier, aujourdhui, t.Unix()); err != nil {
		c.journal.Error("résumé de la semaine", "erreur", err)
		return
	}
	c.informer(ctx, "resume-semaine", c.texteResume(ctx, t))
}

// texteResume : le message, sur les 7 jours qui finissent à fin.
func (c *Console) texteResume(ctx context.Context, fin time.Time) string {
	loc := fuseau.Load()
	debut := fin.Add(-7 * 24 * time.Hour)
	var b strings.Builder
	fmt.Fprintf(&b, "📋 mirador · semaine du %s au %s\n", debut.In(loc).Format("02/01"), fin.Add(-time.Minute).In(loc).Format("02/01"))
	for _, l := range []string{
		c.ligneResumeAlertes(ctx, debut, fin),
		c.ligneResumeDispo(ctx, debut, fin),
		c.ligneResumeDisques(ctx, fin),
		c.ligneResumeTaches(ctx, debut, fin),
		c.ligneResumeMisesAJour(ctx, fin),
		c.ligneResumeFlux(ctx, debut, fin),
	} {
		if l != "" {
			b.WriteString(l + "\n")
		}
	}
	if r, err := notif.LireReglages(ctx, c.db); err == nil && r.Lien != "" {
		b.WriteString(strings.TrimRight(r.Lien, "/") + "/\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// alerteSemaine : une alerte passée en alerte pendant la semaine.
type alerteSemaine struct {
	regle, gravite, machine, objet string
	duree                          time.Duration
}

func (c *Console) alertesSemaine(ctx context.Context, debut, fin time.Time) []alerteSemaine {
	rows, err := c.db.QueryContext(ctx, `SELECT regle, gravite, etiquettes, active_le, fin FROM alertes
		WHERE active_le >= ? AND active_le < ?`, debut.UnixMilli(), fin.UnixMilli())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []alerteSemaine
	for rows.Next() {
		var a alerteSemaine
		var etq string
		var le, f int64
		if rows.Scan(&a.regle, &a.gravite, &etq, &le, &f) != nil {
			continue
		}
		m := map[string]string{}
		_ = json.Unmarshal([]byte(etq), &m)
		for _, k := range []string{"instance", "host", "src_ip"} {
			if m[k] != "" {
				a.machine = m[k]
				break
			}
		}
		for _, k := range []string{"nom", "tache", "name", "id"} {
			if m[k] != "" {
				a.objet = m[k]
				break
			}
		}
		if f == 0 || f > fin.UnixMilli() {
			f = fin.UnixMilli()
		}
		a.duree = time.Duration(f-le) * time.Millisecond
		out = append(out, a)
	}
	return out
}

func (c *Console) ligneResumeAlertes(ctx context.Context, debut, fin time.Time) string {
	as := c.alertesSemaine(ctx, debut, fin)
	if len(as) == 0 {
		return "🔔 Aucune alerte"
	}
	crit := 0
	longue := as[0]
	for _, a := range as {
		if a.gravite == "critical" {
			crit++
		}
		if a.duree > longue.duree {
			longue = a
		}
	}
	s := fmt.Sprintf("🔔 %d %s", len(as), accorder(len(as), "alerte(s)"))
	if crit > 0 {
		s += fmt.Sprintf(" (%d %s)", crit, accorder(crit, "critique(s)"))
	}
	if longue.duree >= time.Minute {
		qui := libelleRegle(longue.regle)
		if longue.machine != "" {
			qui = longue.machine + " · " + qui
		}
		s += " · la plus longue : " + qui + " " + dureeArret(int64(longue.duree.Seconds()))
	}
	return s
}

// ligneResumeDispo : éléments sous 100 % sur la semaine (les 3 pires), même
// calcul que la page Disponibilité (pannes moins maintenances).
func (c *Console) ligneResumeDispo(ctx context.Context, debut, fin time.Time) string {
	r, _ := notif.LireReglages(ctx, c.db)
	pannes := c.pannes(ctx, r.ReglesPanne, debut, fin)
	maint := c.maintenancesMachines(ctx, debut, fin)
	type el struct {
		nom string
		pc  float64
	}
	var els []el
	ajouter := func(cle, nom string, suivi time.Time) {
		d := debut
		if suivi.After(d) {
			d = suivi
		}
		total := fin.Unix() - d.Unix()
		if total <= 0 {
			return
		}
		arret := dureeDans(reunir(retirer(pannes[cle], maint[cle])), d.Unix(), fin.Unix())
		if arret > 0 {
			els = append(els, el{nom, 100 * (1 - float64(arret)/float64(total))})
		}
	}
	nb := 0
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if m.Statut == "approuvee" {
				nb++
				ajouter("m:"+m.Nom, m.Nom, time.Unix(m.CreeLe, 0))
			}
		}
	}
	if ss, err := flotte.LireSondes(ctx, c.db); err == nil {
		for _, s := range ss {
			nb++
			ajouter("s:"+s.Nom, s.Nom, time.Time{})
		}
	}
	if nb == 0 {
		return ""
	}
	if len(els) == 0 {
		return "✅ Disponibilité : 100 % partout"
	}
	sort.Slice(els, func(i, j int) bool { return els[i].pc < els[j].pc })
	var parts []string
	for _, e := range els[:min(3, len(els))] {
		parts = append(parts, e.nom+" "+pct(e.pc)+" %")
	}
	if len(els) > 3 {
		parts = append(parts, "+ "+strconv.Itoa(len(els)-3))
	}
	return "📉 Disponibilité sous 100 % : " + strings.Join(parts, ", ")
}

// ligneResumeDisques : systèmes de fichiers pleins dans moins de 30 jours.
func (c *Console) ligneResumeDisques(ctx context.Context, t time.Time) string {
	p := c.previsionsDisques(ctx, `instance=~".+"`, t)
	type d struct {
		cle   string
		jours float64
	}
	var ds []d
	for k, j := range p {
		if j < 30 {
			ds = append(ds, d{k, j})
		}
	}
	if len(ds) == 0 {
		return ""
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i].jours < ds[j].jours })
	var parts []string
	for _, x := range ds[:min(3, len(ds))] {
		parts = append(parts, strings.Replace(x.cle, "|", " ", 1)+" ("+strconv.Itoa(int(x.jours))+" j)")
	}
	if len(ds) > 3 {
		parts = append(parts, "+ "+strconv.Itoa(len(ds)-3))
	}
	return "💾 Plein dans moins de 30 j : " + strings.Join(parts, ", ")
}

// reglesTaches : règles des tâches planifiées et des sauvegardes.
var reglesTaches = map[string]bool{"TacheEnEchec": true, "TacheEnEchecCritique": true, "TacheEnRetard": true,
	"TacheEnRetardCritique": true, "TacheAnormale": true, "SauvegardeEnEchec": true, "SauvegardeTropAncienne": true}

func (c *Console) ligneResumeTaches(ctx context.Context, debut, fin time.Time) string {
	vus := map[string]bool{}
	var noms []string
	for _, a := range c.alertesSemaine(ctx, debut, fin) {
		if !reglesTaches[a.regle] {
			continue
		}
		n := strings.Trim(a.machine+" · "+a.objet, " ·")
		if n == "" {
			n = libelleRegle(a.regle)
		}
		if !vus[n] {
			vus[n] = true
			noms = append(noms, n)
		}
	}
	if len(noms) == 0 {
		return ""
	}
	sort.Strings(noms)
	s := fmt.Sprintf("🗄️ Tâches et sauvegardes en échec ou en retard : %d (%s", len(noms), strings.Join(noms[:min(3, len(noms))], ", "))
	if len(noms) > 3 {
		s += ", …"
	}
	return s + ")"
}

func (c *Console) ligneResumeMisesAJour(ctx context.Context, t time.Time) string {
	compter := func(expr string) int {
		n := 0
		if es, err := c.instant(ctx, expr, t); err == nil {
			for _, e := range es {
				if e.Valeur > 0 {
					n++
				}
			}
		}
		return n
	}
	sec, redem := compter(exprPaquetsSecurite), compter(exprRedemarrage)
	var parts []string
	if sec > 0 {
		parts = append(parts, fmt.Sprintf("mises à jour de sécurité : %d %s", sec, accorder(sec, "machine(s)")))
	}
	if redem > 0 {
		parts = append(parts, fmt.Sprintf("redémarrage requis : %d", redem))
	}
	if len(parts) == 0 {
		return ""
	}
	return "🔒 " + strings.ToUpper(parts[0][:1]) + parts[0][1:] + strings.Join(append([]string{""}, parts[1:]...), " · ")
}

func (c *Console) ligneResumeFlux(ctx context.Context, debut, fin time.Time) string {
	if c.opt.Flux == nil || c.opt.Flux.Entrants == nil {
		return ""
	}
	b, err := c.opt.Flux.Entrants.Bilan(ctx, debut, fin, fluxbloques.Filtre{}, 1)
	if err != nil || b.Total == 0 {
		return ""
	}
	s := "🧱 Flux bloqués : " + nombreLisible(b.Total)
	if b.AvantConnu && b.Avant > 0 {
		d := (b.Total - b.Avant) * 100 / b.Avant
		signe := "+"
		if d < 0 {
			signe = "−"
			d = -d
		}
		s += " (" + signe + strconv.Itoa(d) + " %)"
	}
	for _, ip := range b.IPs {
		if ip.Nom != fluxbloques.Autres {
			s += " · 1ʳᵉ adresse " + ip.Nom
			break
		}
	}
	return s
}
