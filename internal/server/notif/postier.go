// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/secrets"
)

// Postier (SPEC.md § 17.7) : envoie les messages de l'aiguillage par le bon
// canal, surveille chaque canal, bascule sur le secours quand le principal
// tombe (avec anti-rebond), prévient de la panne et du retour, garde les
// messages en file d'attente sur disque si tout est en panne, et appelle le
// heartbeat externe tant que le serveur évalue ses règles.
//
// Contexte : un jour, les alertes Telegram ont été bloquées par le pare-feu
// sans que personne ne s'en rende compte (brief). D'où la vérification
// régulière de chaque canal, même quand il n'y a rien à envoyer.

// MaxFile : messages gardés au plus dans la file d'attente (les plus anciens
// partent en premier quand elle est pleine).
const MaxFile = 500

// OptionsPostier : dépendances du postier.
type OptionsPostier struct {
	DB                   *sql.DB
	Cle                  []byte
	Journal              *slog.Logger
	Maintenant           func() time.Time
	DerniereEvaluationOK func() time.Time // heartbeat : arrêté si trop ancienne
	// Fabrique remplace la fabrication des canaux (tests : canaux simulés).
	Fabrique func(c *Canaux, cle []byte) (map[string]Canal, error)
}

type etatCanal struct {
	canal          Canal
	enPanne        bool
	echecs, succes int // consécutifs
	verifieLe      time.Time
	depuis         time.Time // dernier changement d'état
	erreur         string
}

// EtatCanal : état affiché (commande notif-etat, métriques internes).
type EtatCanal struct {
	Nom             string
	Role            string // principal, secours, autre
	EnPanne         bool
	Depuis, Verifie time.Time
	Erreur          string
}

// Postier : envois, surveillance, bascule.
type Postier struct {
	opt OptionsPostier

	mu       sync.Mutex
	reglages Reglages
	gen      int64
	etats    map[string]*etatCanal
	hbLe     time.Time // dernier appel du heartbeat
	hbOK     time.Time // dernier appel réussi
	hbErreur string
	hbClient *http.Client

	// Accusés de réception depuis Telegram.
	offset      int64     // prochaine mise à jour à lire
	clicsLe     time.Time // dernière lecture
	minuteClics time.Time
	nbClics     int
}

// RythmeClics : lecture des appuis sur les boutons Telegram.
const RythmeClics = 15 * time.Second

// MaxClicsMinute : acquittements acceptés par minute depuis Telegram.
const MaxClicsMinute = 20

// NouveauPostier crée le postier et lit sa configuration.
func NouveauPostier(ctx context.Context, opt OptionsPostier) (*Postier, error) {
	if opt.Journal == nil {
		opt.Journal = slog.New(slog.DiscardHandler)
	}
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	if opt.Fabrique == nil {
		opt.Fabrique = fabriquer
	}
	p := &Postier{opt: opt, etats: map[string]*etatCanal{}, gen: -1}
	return p, p.recharger(ctx)
}

// recharger relit les réglages et refait les canaux s'ils ont changé ;
// l'état (panne ou non) d'un canal déjà connu est gardé.
func (p *Postier) recharger(ctx context.Context) error {
	gen, err := alertes.LireGeneration(ctx, p.opt.DB, "notif")
	if err != nil {
		return err
	}
	p.mu.Lock()
	inchange := gen == p.gen
	p.mu.Unlock()
	if inchange {
		return nil
	}
	r, err := LireReglages(ctx, p.opt.DB)
	if err != nil {
		return err
	}
	canaux, err := p.opt.Fabrique(&r.Canaux, p.opt.Cle)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Anciens canaux remplacés : connexions libérées (Olvid).
	for _, e := range p.etats {
		if f, ok := e.canal.(interface{ Fermer() }); ok {
			f.Fermer()
		}
	}
	etats := map[string]*etatCanal{}
	for nom, c := range canaux {
		e := p.etats[nom]
		if e == nil {
			e = &etatCanal{depuis: p.opt.Maintenant()}
		}
		e.canal = c
		etats[nom] = e
	}
	p.etats, p.reglages, p.gen = etats, r, gen
	p.hbClient = clientHTTP(r.Canaux.ReseauPrive)
	return nil
}

// ordre : canaux à essayer pour une gravité, dans l'ordre ; tous = envoyer
// sur chacun (gravité de la liste « tous »).
func (p *Postier) ordre(gravite string) (noms []string, tous bool) {
	c := &p.reglages.Canaux
	if slices.Contains(c.Tous, gravite) {
		for nom := range p.etats {
			noms = append(noms, nom)
		}
		sort.Strings(noms)
		return noms, true
	}
	var ok, ko []string
	for _, nom := range []string{c.Principal, c.Secours} {
		e := p.etats[nom]
		switch {
		case nom == "" || e == nil:
		case e.enPanne:
			ko = append(ko, nom)
		default:
			ok = append(ok, nom)
		}
	}
	// Canaux en panne essayés en dernier recours (une panne peut être finie).
	return append(ok, ko...), false
}

// Envoyer : appelé par l'aiguillage (mode actif).
func (p *Postier) Envoyer(ctx context.Context, m *Message, _ *Reglages) {
	if err := p.recharger(ctx); err != nil {
		p.opt.Journal.Error("notifications : réglages", "erreur", err)
	}
	p.envoyer(ctx, m, true)
}

// envoyer : essaie les canaux dans l'ordre ; si tout échoue et enFile, le
// message est gardé dans la file d'attente. Renvoie true si au moins un
// canal l'a pris.
func (p *Postier) envoyer(ctx context.Context, m *Message, enFile bool) bool {
	p.mu.Lock()
	noms, tous := p.ordre(m.Gravite)
	principal := p.reglages.Canaux.Principal
	r := p.reglages
	p.mu.Unlock()
	loc, _ := time.LoadLocation(r.Fuseau)
	envoye := false
	for _, nom := range noms {
		p.mu.Lock()
		e := p.etats[nom]
		p.mu.Unlock()
		copie := *m
		if !tous && nom != principal {
			// Le secours ne sert que si le principal est en panne ou vient
			// d'échouer : le message le dit.
			copie.Secours, copie.CanalEnPanne = nom, principal
		}
		ctxE, annuler := context.WithTimeout(ctx, delaiEnvoi(e.canal))
		t0 := time.Now()
		err := e.canal.Envoyer(ctxE, &copie, Mise{Fuseau: loc, Lien: r.Lien})
		annuler()
		statut, msgErr := "ok", ""
		if err != nil {
			statut, msgErr = "echec", err.Error()
		}
		if errNote := NoterEnvoi(ctx, p.opt.DB, Envoi{Le: p.opt.Maintenant(), Canal: nom, Groupe: m.Groupe, Gravite: m.Gravite,
			Type: m.Type, Nb: m.Nb(), Statut: statut, Duree: time.Since(t0), Secours: !tous && nom != principal,
			Erreur: msgErr, Texte: Texte(&copie, Mise{Fuseau: loc, Lien: r.Lien})}); errNote != nil {
			p.opt.Journal.Error("historique des envois", "erreur", errNote)
		}
		p.noter(ctx, nom, err, false)
		if err == nil {
			envoye = true
			if !tous {
				break
			}
		}
	}
	if !envoye && enFile {
		p.mettreEnFile(ctx, m)
	}
	return envoye
}

// noter : résultat d'une vérification ou d'un envoi, avec anti-rebond.
// Change l'état après Echecs échecs ou Retours réussites de suite, et
// prévient (une fois) de la panne ou du retour.
func (p *Postier) noter(ctx context.Context, nom string, err error, verification bool) {
	p.mu.Lock()
	e := p.etats[nom]
	if e == nil {
		p.mu.Unlock()
		return
	}
	t := p.opt.Maintenant()
	if verification {
		e.verifieLe = t
	}
	c := p.reglages.Canaux
	var transition string
	if err != nil {
		e.echecs, e.succes, e.erreur = e.echecs+1, 0, err.Error()
		if !e.enPanne && e.echecs >= c.Echecs {
			e.enPanne, e.depuis, transition = true, t, "panne"
		}
	} else {
		e.succes, e.echecs = e.succes+1, 0
		if e.enPanne && e.succes >= c.Retours {
			e.enPanne, e.depuis, e.erreur, transition = false, t, "", "retour"
		}
	}
	role := "autre"
	switch nom {
	case c.Principal:
		role = "principal"
	case c.Secours:
		role = "secours"
	}
	erreur := e.erreur
	p.mu.Unlock()
	if transition == "" {
		return
	}
	if transition == "panne" {
		p.opt.Journal.Warn("canal de notification en panne", "canal", nom, "role", role, "erreur", erreur)
	} else {
		p.opt.Journal.Info("canal de notification revenu", "canal", nom, "role", role)
	}
	p.prevenir(ctx, nom, role, transition, erreur, t)
}

// prevenir : message « canal en panne » / « retour à la normale » envoyé par
// les AUTRES canaux (panne) ou par le canal revenu et les autres (retour).
func (p *Postier) prevenir(ctx context.Context, nom, role, transition, erreur string, t time.Time) {
	p.mu.Lock()
	mode := p.reglages.Mode
	var autres []string
	for n, e := range p.etats {
		if n != nom && !e.enPanne {
			autres = append(autres, n)
		}
	}
	p.mu.Unlock()
	sort.Strings(autres)
	texte := fmt.Sprintf("⚠️ Canal %s (%s) en panne : %s\nLes alertes partent par : %s.", nom, role, erreur, listeOuAucun(autres))
	typ, cibles := "panne_canal", autres
	if transition == "retour" {
		texte = fmt.Sprintf("✅ Canal %s (%s) revenu à la normale.", nom, role)
		typ, cibles = "retour_canal", append([]string{nom}, autres...)
	}
	m := &Message{Le: t, Groupe: "canal=" + nom, Gravite: alertes.Critique, Type: typ, Systeme: texte, Son: transition == "panne"}
	if mode == Ombre {
		_ = NoterEnvoi(ctx, p.opt.DB, Envoi{Le: t, Canal: "ombre", Groupe: m.Groupe, Gravite: m.Gravite, Type: typ,
			Statut: Ombre, Texte: texte})
		return
	}
	envoye := false
	for _, n := range cibles {
		envoye = p.envoyerA(ctx, n, m) || envoye
	}
	if !envoye {
		p.mettreEnFile(ctx, m)
	}
}

func listeOuAucun(noms []string) string {
	if len(noms) == 0 {
		return "aucun autre canal (messages gardés en file d'attente)"
	}
	out := ""
	for i, n := range noms {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// envoyerA : un message par un canal précis (messages système, tests).
func (p *Postier) envoyerA(ctx context.Context, nom string, m *Message) bool {
	p.mu.Lock()
	e := p.etats[nom]
	r := p.reglages
	p.mu.Unlock()
	if e == nil {
		return false
	}
	loc, _ := time.LoadLocation(r.Fuseau)
	ctxE, annuler := context.WithTimeout(ctx, delaiEnvoi(e.canal))
	defer annuler()
	t0 := time.Now()
	err := e.canal.Envoyer(ctxE, m, Mise{Fuseau: loc, Lien: r.Lien})
	statut, msgErr := "ok", ""
	if err != nil {
		statut, msgErr = "echec", err.Error()
	}
	_ = NoterEnvoi(ctx, p.opt.DB, Envoi{Le: p.opt.Maintenant(), Canal: nom, Groupe: m.Groupe, Gravite: m.Gravite, Type: m.Type,
		Nb: m.Nb(), Statut: statut, Duree: time.Since(t0), Erreur: msgErr, Texte: Texte(m, Mise{Fuseau: loc})})
	return err == nil
}

// --- File d'attente ----------------------------------------------------------------

func (p *Postier) mettreEnFile(ctx context.Context, m *Message) {
	b, err := json.Marshal(m)
	if err == nil {
		err = func() error {
			tx, err := p.opt.DB.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback() //nolint:errcheck // sans effet après Commit
			if _, err := tx.ExecContext(ctx, `INSERT INTO file_notifications(cree_le, gravite, message) VALUES (?, ?, ?)`,
				ms(m.Le), m.Gravite, string(b)); err != nil {
				return err
			}
			// Bornée : au-delà de MaxFile, les plus anciens sont oubliés.
			if _, err := tx.ExecContext(ctx, `DELETE FROM file_notifications WHERE id NOT IN
				(SELECT id FROM file_notifications ORDER BY id DESC LIMIT ?)`, MaxFile); err != nil {
				return err
			}
			return tx.Commit()
		}()
	}
	if err != nil {
		p.opt.Journal.Error("file d'attente des notifications", "erreur", err)
		return
	}
	_ = NoterEnvoi(ctx, p.opt.DB, Envoi{Le: p.opt.Maintenant(), Canal: "file", Groupe: m.Groupe, Gravite: m.Gravite, Type: m.Type,
		Nb: m.Nb(), Statut: "en_file", Erreur: "tous les canaux sont en panne"})
}

// viderFile renvoie les messages en attente (20 par tour au plus), dans
// l'ordre, avec leur heure d'origine. S'arrête au premier échec.
func (p *Postier) viderFile(ctx context.Context) {
	rows, err := p.opt.DB.QueryContext(ctx, `SELECT id, cree_le, message FROM file_notifications ORDER BY id LIMIT 20`)
	if err != nil {
		p.opt.Journal.Error("file d'attente des notifications", "erreur", err)
		return
	}
	type element struct {
		id, le int64
		m      Message
	}
	var es []element
	for rows.Next() {
		var x element
		var brut string
		if err := rows.Scan(&x.id, &x.le, &brut); err != nil {
			break
		}
		if json.Unmarshal([]byte(brut), &x.m) == nil {
			es = append(es, x)
		}
	}
	_ = rows.Close()
	for _, x := range es {
		x.m.EnFileDepuis = depuisMs(x.le)
		if !p.envoyer(ctx, &x.m, false) {
			return
		}
		if _, err := p.opt.DB.ExecContext(ctx, `DELETE FROM file_notifications WHERE id = ?`, x.id); err != nil {
			p.opt.Journal.Error("file d'attente des notifications", "erreur", err)
			return
		}
	}
}

// TailleFile : messages en attente.
func (p *Postier) TailleFile(ctx context.Context) int {
	var n int
	_ = p.opt.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_notifications`).Scan(&n)
	return n
}

// --- Boucle : vérifications, file, tests, heartbeat --------------------------------

// Boucle : un tour toutes les 5 s jusqu'à l'arrêt.
func (p *Postier) Boucle(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tour(ctx)
		}
	}
}

// Tour : vérifie les canaux arrivés à échéance, vide la file si un canal
// marche, fait les tests demandés, appelle le heartbeat.
func (p *Postier) Tour(ctx context.Context) {
	if err := p.recharger(ctx); err != nil {
		p.opt.Journal.Error("notifications : réglages", "erreur", err)
	}
	t := p.opt.Maintenant()
	p.mu.Lock()
	c := p.reglages.Canaux
	var dus []string
	for nom, e := range p.etats {
		iv := time.Duration(c.VerifAutres)
		if nom == NomTelegram && !e.enPanne {
			iv = time.Duration(c.VerifTelegram)
		}
		if t.Sub(e.verifieLe) >= iv {
			dus = append(dus, nom)
		}
	}
	unMarche := false
	for _, e := range p.etats {
		unMarche = unMarche || !e.enPanne
	}
	p.mu.Unlock()
	sort.Strings(dus)
	for _, nom := range dus {
		p.mu.Lock()
		canal := p.etats[nom].canal
		p.mu.Unlock()
		ctxV, annuler := context.WithTimeout(ctx, 20*time.Second)
		err := canal.Verifier(ctxV)
		annuler()
		p.noter(ctx, nom, err, true)
	}
	if unMarche && p.TailleFile(ctx) > 0 {
		p.viderFile(ctx)
	}
	p.faireTests(ctx)
	p.lireClics(ctx, t)
	p.battre(ctx, t)
}

// lireClics : appuis sur « Acquitter » sous les messages Telegram (SPEC
// § 17.10). Seuls les appuis venant du groupe configuré, et des personnes
// autorisées si la liste est remplie, sont pris en compte ; 20 par minute
// au plus. Le bot ne lit jamais les messages du groupe.
func (p *Postier) lireClics(ctx context.Context, t time.Time) {
	p.mu.Lock()
	cfg := p.reglages.Canaux.Telegram
	e := p.etats[NomTelegram]
	du := cfg != nil && cfg.Acquitter && e != nil && !e.enPanne && t.Sub(p.clicsLe) >= RythmeClics
	if du {
		p.clicsLe = t
	}
	offset := p.offset
	p.mu.Unlock()
	if !du {
		return
	}
	tg, ok := e.canal.(*telegram)
	if !ok {
		return
	}
	chat, _ := strconv.ParseInt(cfg.Chat, 10, 64)
	ctxL, annuler := context.WithTimeout(ctx, 20*time.Second)
	defer annuler()
	clics, err := tg.clics(ctxL, offset)
	if err != nil {
		p.opt.Journal.Warn("lecture des boutons Telegram", "erreur", err)
		return
	}
	for _, c := range clics {
		p.mu.Lock()
		if c.UpdateID >= p.offset {
			p.offset = c.UpdateID + 1
		}
		p.mu.Unlock()
		cb := c.Callback
		if cb == nil || cb.Message == nil {
			continue
		}
		if cb.Message.Chat.ID != chat {
			continue // bouton d'un autre groupe : ignoré, sans réponse
		}
		if len(cfg.Acquitteurs) > 0 && !slices.Contains(cfg.Acquitteurs, cb.De.ID) {
			tg.repondreClic(ctxL, cb.ID, "Vous n'êtes pas autorisé à acquitter.", 0, 0, false)
			p.opt.Journal.Warn("acquittement Telegram refusé : personne non autorisée", "telegram_id", cb.De.ID)
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(cb.Data, "ack:%d", &id); err != nil || id <= 0 {
			continue
		}
		if !p.compterClic(t) {
			tg.repondreClic(ctxL, cb.ID, "Trop d'acquittements : réessayez dans une minute.", 0, 0, false)
			continue
		}
		qui := fmt.Sprintf("telegram:%d", cb.De.ID)
		if cb.De.Username != "" {
			qui += " (@" + cb.De.Username + ")"
		}
		if err := Acquitter(ctx, p.opt.DB, id, qui, "depuis Telegram", p.opt.Maintenant()); err != nil {
			tg.repondreClic(ctxL, cb.ID, "Alerte déjà finie ou inconnue.", 0, 0, false)
			continue
		}
		p.opt.Journal.Info("alerte acquittée depuis Telegram", "alerte", id, "qui", qui)
		tg.repondreClic(ctxL, cb.ID, fmt.Sprintf("Alerte n° %d acquittée : plus de rappel.", id), cb.Message.Chat.ID, cb.Message.ID, true)
	}
}

// compterClic : limite de débit des acquittements depuis Telegram.
func (p *Postier) compterClic(t time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t.Sub(p.minuteClics) >= time.Minute {
		p.minuteClics, p.nbClics = t, 0
	}
	p.nbClics++
	return p.nbClics <= MaxClicsMinute
}

// faireTests : demandes « notif-tester » déposées en base par la commande.
func (p *Postier) faireTests(ctx context.Context) {
	rows, err := p.opt.DB.QueryContext(ctx, `SELECT id, canal FROM tests_canaux WHERE fait_le = 0 ORDER BY id LIMIT 5`)
	if err != nil {
		return
	}
	type demande struct {
		id    int64
		canal string
	}
	var ds []demande
	for rows.Next() {
		var d demande
		if rows.Scan(&d.id, &d.canal) == nil {
			ds = append(ds, d)
		}
	}
	_ = rows.Close()
	for _, d := range ds {
		statut, msgErr := "ok", ""
		p.mu.Lock()
		e := p.etats[d.canal]
		p.mu.Unlock()
		if e == nil {
			statut, msgErr = "echec", "canal non configuré ou désactivé"
		} else {
			t := p.opt.Maintenant()
			m := &Message{Le: t, Groupe: "test", Gravite: alertes.Info, Type: "test",
				Systeme: "🧪 Message de test de mirador-server (canal " + d.canal + ")."}
			if !p.envoyerA(ctx, d.canal, m) {
				statut, msgErr = "echec", "envoi refusé (voir la commande envois)"
			}
		}
		_, _ = p.opt.DB.ExecContext(ctx, `UPDATE tests_canaux SET fait_le = ?, statut = ?, erreur = ? WHERE id = ?`,
			ms(p.opt.Maintenant()), statut, msgErr, d.id)
	}
}

// battre : heartbeat externe (HetrixTools), seulement si les règles ont été
// évaluées sans panne du stockage depuis moins de 3 intervalles.
func (p *Postier) battre(ctx context.Context, t time.Time) {
	p.mu.Lock()
	h := p.reglages.Canaux.Heartbeat
	client := p.hbClient
	du := h != nil && h.Actif && h.URL != "" && t.Sub(p.hbLe) >= time.Duration(h.Intervalle)
	p.mu.Unlock()
	if !du {
		return
	}
	iv := time.Duration(h.Intervalle)
	p.mu.Lock()
	p.hbLe = t
	p.mu.Unlock()
	if ok := p.opt.DerniereEvaluationOK(); t.Sub(ok) > 3*max(iv, time.Minute) {
		p.mu.Lock()
		p.hbErreur = "suspendu : les règles ne sont plus évaluées (stockage en panne ?)"
		p.mu.Unlock()
		p.opt.Journal.Warn("heartbeat suspendu : évaluation des règles en échec", "derniere_ok", ok)
		return
	}
	url, err := secrets.Dechiffrer(p.opt.Cle, usageHeartbeat, h.URL)
	if err == nil {
		err = appelerHeartbeat(ctx, client, url)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.hbErreur = err.Error()
		p.opt.Journal.Warn("heartbeat en échec", "erreur", err)
		return
	}
	p.hbOK, p.hbErreur = t, ""
}

// TesterHeartbeat : appel immédiat de l'adresse enregistrée (bouton
// « Tester » de l'interface). Le service reçoit un « vivant » de plus.
func (p *Postier) TesterHeartbeat(ctx context.Context) error {
	p.mu.Lock()
	h, client := p.reglages.Canaux.Heartbeat, p.hbClient
	p.mu.Unlock()
	if h == nil || h.URL == "" || client == nil {
		return errors.New("heartbeat : aucune adresse enregistrée (enregistrer d'abord)")
	}
	url, err := secrets.Dechiffrer(p.opt.Cle, usageHeartbeat, h.URL)
	if err != nil {
		return err
	}
	return appelerHeartbeat(ctx, client, url)
}

func appelerHeartbeat(ctx context.Context, client *http.Client, url string) error {
	ctx, annuler := context.WithTimeout(ctx, 10*time.Second)
	defer annuler()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("heartbeat : adresse invalide")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat : %w", sansSecret(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("heartbeat : HTTP %d", resp.StatusCode)
	}
	return nil
}

// Etat : état de chaque canal et du heartbeat.
func (p *Postier) Etat() (canaux []EtatCanal, hbOK time.Time, hbErreur string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.reglages.Canaux
	for nom, e := range p.etats {
		role := "autre"
		switch nom {
		case c.Principal:
			role = "principal"
		case c.Secours:
			role = "secours"
		}
		canaux = append(canaux, EtatCanal{Nom: nom, Role: role, EnPanne: e.enPanne, Depuis: e.depuis, Verifie: e.verifieLe, Erreur: e.erreur})
	}
	sort.Slice(canaux, func(i, j int) bool { return canaux[i].Nom < canaux[j].Nom })
	return canaux, p.hbOK, p.hbErreur
}

// DemanderTest dépose une demande de test (commande) ; le serveur en marche
// l'exécute à son prochain tour (5 s). Renvoie le numéro de la demande.
func DemanderTest(ctx context.Context, db *sql.DB, canal string, t time.Time) (int64, error) {
	if !slices.Contains(nomsCanaux(), canal) {
		return 0, fmt.Errorf("canal %q inconnu", canal)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO tests_canaux(canal, demande_le) VALUES (?, ?)`, canal, ms(t))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ResultatTest : statut ("" = pas encore fait) et erreur d'une demande de test.
func ResultatTest(ctx context.Context, db *sql.DB, id int64) (statut, erreur string, err error) {
	err = db.QueryRowContext(ctx, `SELECT statut, erreur FROM tests_canaux WHERE id = ?`, id).Scan(&statut, &erreur)
	return statut, erreur, err
}

// delaiEnvoi : 30 s par envoi, plus pour un canal qui attend une
// confirmation (Olvid).
func delaiEnvoi(c Canal) time.Duration {
	if d, ok := c.(interface{ DelaiEnvoi() time.Duration }); ok {
		return d.DelaiEnvoi()
	}
	return 30 * time.Second
}
