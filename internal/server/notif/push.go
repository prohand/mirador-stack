// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/webpush"
)

// Canal « application » (SPEC § 35) : notifications envoyées aux appareils
// où un utilisateur a installé mirador et accepté les notifications
// (« Mon compte »). Le message passe par le service de push du navigateur
// (Google, Apple, Mozilla), chiffré pour l'appareil : ce service ne peut pas
// le lire. Il porte aussi le nombre d'alertes en cours, affiché en pastille
// sur l'icône de l'application.
//
// Deux usages :
//   - canal principal ou de secours, comme Telegram ou ntfy ;
//   - « copie » (réglage, coché par défaut) : quand un autre canal porte le
//     message, les appareils le reçoivent aussi. La copie ne compte pas pour
//     la bascule : un téléphone éteint ne met aucun canal « en panne ».

// NomApplication : nom du canal.
const NomApplication = "application"

// Application : réglages du canal.
type Application struct {
	Actif bool `json:"actif"`
	// Copie : envoyer aussi aux appareils quand un autre canal porte le
	// message (sans effet si l'application est elle-même le canal utilisé).
	Copie bool `json:"copie"`
}

// Bornes.
const (
	// MaxAppareilsPush : appareils abonnés par compte.
	MaxAppareilsPush = 10
	// echecsPushMax : envois ratés de suite avant d'effacer un abonnement
	// (appareil perdu, clé du serveur changée).
	echecsPushMax = 30
	// envoisPushEnMemeTemps : appareils servis en parallèle.
	envoisPushEnMemeTemps = 4
)

const (
	usageVAPID = "notif.push.vapid" // #nosec G101 -- nom d'usage du chiffrement, pas un secret
	cleVAPID   = "notif.push.vapid" // #nosec G101 -- clé du réglage en base
)

// clesVAPID : ce qui est gardé en base (clé privée chiffrée).
type clesVAPID struct {
	Prive    string `json:"prive"`    // PKCS#8, base64, chiffré
	Publique string `json:"publique"` // point P-256 non compressé, base64 « URL »
}

var muVAPID sync.Mutex

// ClePush rend la clé du serveur pour les notifications push (VAPID) et sa
// partie publique, donnée aux navigateurs. Créée au premier besoin ; la clé
// privée est gardée chiffrée avec la clé des secrets.
//
// Si elle change (base restaurée sans elle, clé des secrets changée), les
// appareils déjà abonnés ne reçoivent plus rien : il faut les réabonner.
func ClePush(ctx context.Context, db *sql.DB, cle []byte, t time.Time) (*ecdsa.PrivateKey, string, error) {
	muVAPID.Lock()
	defer muVAPID.Unlock()
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleVAPID).Scan(&v)
	if err == nil {
		var k clesVAPID
		if json.Unmarshal([]byte(v), &k) != nil {
			return nil, "", errors.New("clé des notifications push illisible en base")
		}
		clair, err := secrets.Dechiffrer(cle, usageVAPID, k.Prive)
		if err != nil {
			return nil, "", fmt.Errorf("clé des notifications push : %w", err)
		}
		der, err := base64.StdEncoding.DecodeString(clair)
		if err != nil {
			return nil, "", errors.New("clé des notifications push illisible")
		}
		x, err := x509.ParsePKCS8PrivateKey(der)
		priv, ok := x.(*ecdsa.PrivateKey)
		if err != nil || !ok {
			return nil, "", errors.New("clé des notifications push illisible")
		}
		return priv, k.Publique, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	priv, err := webpush.NouvelleCle()
	if err != nil {
		return nil, "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", err
	}
	chiffre, err := secrets.Chiffrer(cle, usageVAPID, base64.StdEncoding.EncodeToString(der))
	if err != nil {
		return nil, "", err
	}
	pub, err := webpush.ClePublique(priv)
	if err != nil {
		return nil, "", err
	}
	brut, _ := json.Marshal(clesVAPID{Prive: chiffre, Publique: pub})
	if _, err := db.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, 'mirador')`,
		cleVAPID, string(brut), t.Unix()); err != nil {
		return nil, "", err
	}
	return priv, pub, nil
}

// --- Appareils abonnés ---------------------------------------------------------------

// AppareilPush : un appareil abonné (page « Mon compte »).
type AppareilPush struct {
	ID            int64
	UtilisateurID int64
	Appareil      string // « iPhone », « Chrome sur Android »…
	Service       string // hôte du service de push
	Empreinte     string // début du SHA-256 de l'adresse : reconnaître « cet appareil » sans montrer l'adresse
	CreeLe        int64
	ReussiLe      int64
	Echecs        int

	abonnement webpush.Abonnement
	origine    string
}

func empreinteAdresse(adresse string) string {
	h := sha256.Sum256([]byte(adresse))
	return fmt.Sprintf("%x", h[:8])
}

// AbonnerPush enregistre un appareil pour ce compte. Un appareil déjà connu
// (même adresse) est repris, même s'il était lié à un autre compte : c'est
// la personne qui tient l'appareil qui choisit. origine : « https://hôte »
// de l'interface au moment de l'abonnement (contact annoncé au service de
// push).
func AbonnerPush(ctx context.Context, db *sql.DB, utilisateur int64, a webpush.Abonnement, origine, appareil string, t time.Time) error {
	if err := a.Valider(); err != nil {
		return err
	}
	if !strings.HasPrefix(origine, "https://") || len(origine) > 300 {
		return errors.New("les notifications demandent l'interface en HTTPS, par son nom de domaine")
	}
	if r := []rune(appareil); len(r) > 60 {
		appareil = string(r[:60])
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM abonnements_push WHERE utilisateur_id = ? AND adresse != ?`,
		utilisateur, a.Adresse).Scan(&n); err != nil {
		return err
	}
	if n >= MaxAppareilsPush {
		return fmt.Errorf("%d appareils au plus par compte : en retirer un d'abord", MaxAppareilsPush)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO abonnements_push(utilisateur_id, adresse, p256dh, auth, origine, appareil, cree_le)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(adresse) DO UPDATE SET utilisateur_id = excluded.utilisateur_id, p256dh = excluded.p256dh, auth = excluded.auth,
			origine = excluded.origine, appareil = excluded.appareil, echecs = 0`,
		utilisateur, a.Adresse, a.P256dh, a.Auth, origine, appareil, t.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// listerPush : appareils d'un compte (utilisateur > 0) ou de tous les
// comptes actifs (utilisateur = 0).
func listerPush(ctx context.Context, db *sql.DB, utilisateur int64) ([]AppareilPush, error) {
	q := `SELECT a.id, a.utilisateur_id, a.adresse, a.p256dh, a.auth, a.origine, a.appareil, a.cree_le, a.reussi_le, a.echecs
		FROM abonnements_push a JOIN utilisateurs u ON u.id = a.utilisateur_id WHERE u.desactive = 0`
	args := []any{}
	if utilisateur > 0 {
		q += ` AND a.utilisateur_id = ?`
		args = append(args, utilisateur)
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY a.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppareilPush
	for rows.Next() {
		var p AppareilPush
		if err := rows.Scan(&p.ID, &p.UtilisateurID, &p.abonnement.Adresse, &p.abonnement.P256dh, &p.abonnement.Auth, &p.origine,
			&p.Appareil, &p.CreeLe, &p.ReussiLe, &p.Echecs); err != nil {
			return nil, err
		}
		p.Empreinte = empreinteAdresse(p.abonnement.Adresse)
		if i := strings.Index(p.abonnement.Adresse, "://"); i > 0 {
			p.Service = strings.SplitN(p.abonnement.Adresse[i+3:], "/", 2)[0]
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListerAppareilsPush : appareils abonnés d'un compte.
func ListerAppareilsPush(ctx context.Context, db *sql.DB, utilisateur int64) ([]AppareilPush, error) {
	return listerPush(ctx, db, utilisateur)
}

// NombreAppareilsPush : appareils abonnés, tous comptes actifs confondus.
func NombreAppareilsPush(ctx context.Context, db *sql.DB) int {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM abonnements_push a JOIN utilisateurs u ON u.id = a.utilisateur_id WHERE u.desactive = 0`).Scan(&n)
	return n
}

// RetirerAppareilPush efface un appareil du compte (rien s'il n'est pas à lui).
func RetirerAppareilPush(ctx context.Context, db *sql.DB, utilisateur, id int64) (string, error) {
	var nom string
	err := db.QueryRowContext(ctx, `SELECT appareil FROM abonnements_push WHERE id = ? AND utilisateur_id = ?`, id, utilisateur).Scan(&nom)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("appareil inconnu")
	}
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM abonnements_push WHERE id = ? AND utilisateur_id = ?`, id, utilisateur)
	return nom, err
}

// --- Envoi ---------------------------------------------------------------------------

// ChargePush : ce que reçoit l'appareil (lu par sw.js).
type ChargePush struct {
	Titre    string `json:"titre"`
	Corps    string `json:"corps"`
	Lien     string `json:"lien"`     // page ouverte au toucher : chemin de l'interface
	Pastille int    `json:"pastille"` // alertes en cours (pastille de l'icône) ; -1 = ne pas toucher
	Groupe   string `json:"groupe"`   // une notification du même groupe remplace la précédente
	Son      bool   `json:"son"`
}

// alertesEnCours : nombre montré en pastille (actives, pas prises en charge).
func alertesEnCours(ctx context.Context, db *sql.DB) int {
	var n int
	if db.QueryRowContext(ctx, `SELECT count(*) FROM alertes WHERE etat = 'active' AND acquittee_le = 0`).Scan(&n) != nil {
		return -1
	}
	return n
}

// chargeDe : message de mirador → notification de l'appareil.
func chargeDe(m *Message, o Mise, pastille int) ChargePush {
	c := ChargePush{Titre: "mirador", Lien: "/alertes", Pastille: pastille, Groupe: m.Groupe, Son: m.Son}
	switch {
	case m.Systeme != "":
		c.Lien = "/"
	case m.Site != "":
		c.Titre, c.Lien = m.TitreSite(), "/coupures?site="+url.QueryEscape(m.Site)
	case len(m.Actives) > 0:
		c.Titre = alertes.Libelle(m.Actives[0].Regle)
		if h := m.Actives[0].Machine(); h != "" {
			c.Titre += " · " + h
		}
		if n := m.Nb(); n > 1 {
			c.Titre += fmt.Sprintf(" (+%d)", n-1)
		} else if m.Actives[0].ID > 0 {
			c.Lien = fmt.Sprintf("/alertes/%d", m.Actives[0].ID)
		}
	case len(m.Resolues) > 0:
		c.Titre = "Résolu : " + alertes.Libelle(m.Resolues[0].Regle)
		if h := m.Resolues[0].Machine(); h != "" {
			c.Titre += " · " + h
		}
	}
	// Texte sans lien (le toucher ouvre la page) et court : un écran de
	// téléphone, et un corps chiffré bien sous la limite des services.
	o.HTML, o.Lien, o.Max, o.TitreAPart = false, "", corpsPushMax, true
	if t := TitreModele(m, o); t != "" { // modèle de l'administrateur (SPEC § 82)
		c.Titre = t
	}
	c.Corps = Texte(m, o)
	// Borne tenue ici aussi (un message système n'est pas coupé par Texte) :
	// le corps chiffré doit rester sous la limite des services de push.
	if r := []rune(c.Corps); len(r) > corpsPushMax {
		c.Corps = string(r[:corpsPushMax-1]) + "…"
	}
	if len(c.Groupe) > 200 {
		c.Groupe = c.Groupe[:200]
	}
	if r := []rune(c.Titre); len(r) > 120 {
		c.Titre = string(r[:119]) + "…"
	}
	return c
}

// corpsPushMax : signes du texte d'une notification (600 signes font au
// plus 2 400 octets : sous webpush.TailleMaxMessage avec le titre).
const corpsPushMax = 600

// push : le canal.
type push struct {
	db         *sql.DB
	cle        []byte
	client     *http.Client
	maintenant func() time.Time
}

func (p *push) Nom() string { return NomApplication }

// DelaiEnvoi : plusieurs appareils, servis quatre par quatre.
func (p *push) DelaiEnvoi() time.Duration { return 40 * time.Second }

// Verifier : la clé du serveur est lisible. Rien n'est envoyé ; un appareil
// éteint ou absent n'est pas une panne du canal.
func (p *push) Verifier(ctx context.Context) error {
	_, _, err := ClePush(ctx, p.db, p.cle, p.maintenant())
	return err
}

// Envoyer : à tous les appareils des comptes actifs. Réussi si au moins un
// appareil l'a reçu ; échec s'il n'y en a aucun (compte pour la bascule
// quand l'application est le canal principal ou de secours).
func (p *push) Envoyer(ctx context.Context, m *Message, o Mise) error {
	appareils, err := listerPush(ctx, p.db, 0)
	if err != nil {
		return errors.New("application : liste des appareils illisible")
	}
	if len(appareils) == 0 {
		return errors.New("application : aucun appareil abonné (« Mon compte » › Notifications sur cet appareil)")
	}
	ok, dernier := p.envoyerA(ctx, appareils, chargeDe(m, o, alertesEnCours(ctx, p.db)), m.Gravite == alertes.Critique)
	if ok == 0 {
		return fmt.Errorf("application : aucun des %d appareils n'a pu être joint (%v)", len(appareils), dernier)
	}
	return nil
}

// envoyerA : la notification à ces appareils ; renvoie le nombre de réussites
// et la dernière erreur. Un abonnement expiré est effacé.
func (p *push) envoyerA(ctx context.Context, appareils []AppareilPush, c ChargePush, urgent bool) (int, error) {
	priv, _, err := ClePush(ctx, p.db, p.cle, p.maintenant())
	if err != nil {
		return 0, err
	}
	clair, err := json.Marshal(c)
	if err != nil {
		return 0, err
	}
	// Groupe du service de push (« Topic », 32 signes au plus) : si
	// l'appareil est éteint, seul le dernier message du groupe est gardé.
	h := sha256.Sum256([]byte(c.Groupe))
	groupe := base64.RawURLEncoding.EncodeToString(h[:18])
	var (
		mu      sync.Mutex
		reussis int
		dernier error
		wg      sync.WaitGroup
	)
	places := make(chan struct{}, envoisPushEnMemeTemps)
	for i := range appareils {
		a := &appareils[i]
		wg.Add(1)
		places <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-places }()
			err := webpush.Envoyer(ctx, p.client, &a.abonnement, clair, webpush.Options{Cle: priv, Sujet: a.origine,
				Urgent: urgent, Groupe: groupe, Instant: p.maintenant()})
			switch {
			case err == nil:
				_, _ = p.db.ExecContext(ctx, `UPDATE abonnements_push SET reussi_le = ?, echecs = 0 WHERE id = ?`, p.maintenant().Unix(), a.ID)
			case errors.Is(err, webpush.ErrExpire) || errors.Is(err, webpush.ErrAbonnement):
				_, _ = p.db.ExecContext(ctx, `DELETE FROM abonnements_push WHERE id = ?`, a.ID)
			default:
				// Autre erreur (réseau, service saturé) : gardé, puis effacé
				// après trop d'échecs de suite.
				_, _ = p.db.ExecContext(ctx, `UPDATE abonnements_push SET echecs = echecs + 1 WHERE id = ?`, a.ID)
				_, _ = p.db.ExecContext(ctx, `DELETE FROM abonnements_push WHERE id = ? AND echecs >= ?`, a.ID, echecsPushMax)
			}
			mu.Lock()
			if err == nil {
				reussis++
			} else {
				dernier = err
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return reussis, dernier
}

// ClientPush : client HTTP protégé (pas de redirection, pas d'adresse
// interne) ; remplacé dans les tests par un faux service de push.
var ClientPush = func() *http.Client { return clientHTTP(false) }

// nouveauPush : canal « application ».
func nouveauPush(db *sql.DB, cle []byte, maintenant func() time.Time) *push {
	return &push{db: db, cle: cle, client: ClientPush(), maintenant: maintenant}
}

// TesterPush envoie une notification d'essai aux appareils d'un compte
// (bouton « Tester » de « Mon compte »). Renvoie le nombre d'appareils joints.
func TesterPush(ctx context.Context, db *sql.DB, cle []byte, utilisateur int64, t time.Time) (int, error) {
	appareils, err := listerPush(ctx, db, utilisateur)
	if err != nil {
		return 0, err
	}
	if len(appareils) == 0 {
		return 0, errors.New("aucun appareil abonné pour ce compte")
	}
	p := nouveauPush(db, cle, func() time.Time { return t })
	ctx, annuler := context.WithTimeout(ctx, 20*time.Second)
	defer annuler()
	n, dernier := p.envoyerA(ctx, appareils, ChargePush{Titre: "mirador", Corps: "🧪 Notification d'essai : cet appareil reçoit bien les alertes.",
		Lien: "/compte#push", Pastille: alertesEnCours(ctx, db), Groupe: "test"}, true)
	if n == 0 {
		return 0, fmt.Errorf("aucun appareil joint (%v)", dernier)
	}
	return n, nil
}
