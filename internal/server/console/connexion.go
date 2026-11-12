// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"crypto/subtle"
	"errors"
	"html/template"
	"mirador-stack/internal/server/auth"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/web"
)

// Connexion en deux temps : mot de passe, puis code 2FA (ou création de la
// 2FA si le compte n'en a pas encore). Chaque échec est écrit dans le
// journal du serveur sous une forme fixe, lue par CrowdSec :
//
//	{"level":"WARN","msg":"console : échec de connexion","ip":"…","utilisateur":"…","raison":"…"}

const usageTOTP = "console.totp"

func (c *Console) dechiffrerTOTP(v string) (string, error) {
	return secrets.Dechiffrer(c.opt.Cle, usageTOTP, v)
}

// donneesConnexion : ce qu'affichent les pages de connexion.
type donneesConnexion struct {
	Nom, Erreur, Secret, Jeton string
	QR                         template.URL
	Premier                    bool
	// Passkeys du compte utilisables sous ce domaine (étape du code) : défi,
	// domaine et identifiants (séparés par des virgules), pour passkey.js.
	PKDefi, PKDomaine, PKIds string
}

func (c *Console) pageConnexion(w http.ResponseWriter, r *http.Request) {
	if c.premierAFaire() {
		http.Redirect(w, r, "/premier-admin", http.StatusSeeOther)
		return
	}
	c.pages.rendreSeule(w, "connexion", "Connexion", donneesConnexion{})
}

// premierAFaire : vrai tant qu'aucun compte n'existe. Un compte créé
// autrement (base restaurée, autre processus) annule le jeton tout de suite.
func (c *Console) premierAFaire() bool {
	c.muPremier.Lock()
	defer c.muPremier.Unlock()
	if c.jetonPremier == "" {
		return false
	}
	if n, err := auth.NombreUtilisateurs(context.Background(), c.db); err == nil && n > 0 {
		c.jetonPremier = ""
		return false
	}
	return true
}

// nomJournal : nom écrit dans le journal. Un texte qui n'a pas la forme
// d'un nom de compte n'est jamais écrit : c'est souvent un mot de passe
// tapé par erreur dans le champ « Nom » (le journal est lu par CrowdSec).
func nomJournal(nom string) string {
	if nom == "" || auth.VerifierNom(nom) == nil {
		return nom
	}
	return "(invalide)"
}

// echecConnexion : compteurs de verrouillage + ligne de journal (CrowdSec).
func (c *Console) echecConnexion(ip, nom, raison string, t time.Time) {
	sec := c.securite.Load()
	c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", nomJournal(nom), "raison", raison)
	if d := c.verrous.EchecAdresse(ip, sec.Echecs, time.Duration(sec.Verrou), t); d > 0 {
		c.journal.Warn("console : adresse verrouillée", "ip", ip, "duree", d.String())
	}
	if nom == "" {
		return
	}
	if d := c.verrous.EchecCompte(nom, 3*sec.Echecs, time.Duration(sec.Verrou), t); d > 0 {
		c.journal.Warn("console : compte verrouillé", "ip", ip, "utilisateur", nomJournal(nom), "duree", d.String())
		c.prevenirVerrou(nom, ip, d, t)
	}
}

const (
	messageEchec      = "Nom ou mot de passe faux"
	messageSimultanes = "trop d'essais en même temps : réessayer dans un instant"
)

func (c *Console) postConnexion(w http.ResponseWriter, r *http.Request) {
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	nom := strings.TrimSpace(r.PostFormValue("nom"))
	mdp := r.PostFormValue("mot_de_passe")
	d := donneesConnexion{Nom: nom}
	if len(nom) > 64 {
		nom = nom[:64]
	}
	if c.verrous.BloqueAdresse(ip, t) {
		d.Erreur = "trop d'essais depuis cette adresse : réessayer plus tard"
		c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", nomJournal(nom), "raison", "adresse verrouillée")
		c.pages.rendreSeuleCode(w, http.StatusTooManyRequests, "connexion", "Connexion", d)
		return
	}
	if c.verrous.BloqueCompte(nom, ip, t) {
		d.Erreur = "trop d'essais sur ce compte : réessayer plus tard"
		c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", nomJournal(nom), "raison", "compte verrouillé")
		c.pages.rendreSeuleCode(w, http.StatusTooManyRequests, "connexion", "Connexion", d)
		return
	}
	// Essai réservé avant le calcul : une rafale de requêtes simultanées
	// ne donne pas plus d'essais que le seuil (voir verrou.go).
	finir, ok := c.verrous.Debuter(nom, ip, c.securite.Load().Echecs, t)
	if !ok {
		d.Erreur = messageSimultanes
		c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", nomJournal(nom), "raison", "trop d'essais en même temps")
		c.pages.rendreSeuleCode(w, http.StatusTooManyRequests, "connexion", "Connexion", d)
		return
	}
	defer finir()
	u, err := auth.LireUtilisateur(r.Context(), c.db, nom)
	if err != nil && !errors.Is(err, auth.ErrUtilisateurInconnu) {
		http.Error(w, "base indisponible", http.StatusServiceUnavailable)
		return
	}
	empreinte := u.MotDePasse
	if u.ID == 0 {
		empreinte = auth.EmpreinteLeurre // même durée de calcul qu'un vrai compte
	}
	bon, occupe := auth.ComparerOccupe(empreinte, mdp)
	if occupe {
		d.Erreur = auth.ErrOccupe.Error()
		c.pages.rendreSeuleCode(w, http.StatusServiceUnavailable, "connexion", "Connexion", d)
		return
	}
	if u.ID == 0 || !bon || u.Desactive {
		raison := "mot de passe faux"
		switch {
		case u.ID == 0:
			raison = "compte inconnu"
		case u.Desactive && bon:
			raison = "compte désactivé"
		}
		c.echecConnexion(ip, nom, raison, t)
		d.Erreur = messageEchec
		c.pages.rendreSeuleCode(w, http.StatusUnauthorized, "connexion", "Connexion", d)
		return
	}
	etape := auth.EtapeCode
	if u.TOTP == "" {
		etape = auth.EtapeCreer
	}
	// Nouvelle session à chaque connexion : une ancienne session (même
	// partielle) n'est jamais reprise.
	if ck, _ := r.Cookie(auth.NomCookie); ck != nil {
		if s := c.sessions.Lire(ck.Value); s != nil {
			c.sessions.Supprimer(s.Empreinte)
		}
	}
	id := c.sessions.Creer(&auth.Session{UtilisateurID: u.ID, Nom: u.Nom, Generation: u.Generation, Etape: etape, Adresse: ip}, t)
	auth.PoserCookie(w, id)
	http.Redirect(w, r, cheminEtape(etape), http.StatusSeeOther)
}

// sessionEtape : session partielle à l'étape attendue, sinon retour au début.
func (c *Console) sessionEtape(w http.ResponseWriter, r *http.Request, etape string) *auth.Session {
	ck, _ := r.Cookie(auth.NomCookie)
	if ck == nil {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return nil
	}
	s := c.sessions.Lire(ck.Value)
	if s == nil || s.Etape != etape || c.maintenant().Sub(s.Creee) > auth.DureeAvantCode {
		if s != nil {
			c.sessions.Supprimer(s.Empreinte)
		}
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return nil
	}
	// Formulaires des étapes : jeton CSRF de la session partielle.
	if r.Method == http.MethodPost && subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
		http.Error(w, "jeton de formulaire invalide : recommencer", http.StatusForbidden)
		return nil
	}
	return s
}

func (c *Console) pageCode(w http.ResponseWriter, r *http.Request) {
	s := c.sessionEtape(w, r, auth.EtapeCode)
	if s == nil {
		return
	}
	c.rendreEtapeCode(w, r, http.StatusOK, s, "")
}

// rendreEtapeCode affiche la page de l'étape 2FA : code TOTP, passkey (si le
// compte en a pour ce domaine : un nouveau défi est préparé) ou code de
// secours.
func (c *Console) rendreEtapeCode(w http.ResponseWriter, r *http.Request, code int, s *auth.Session, erreur string) {
	d := donneesConnexion{Nom: s.Nom, Jeton: s.CSRF, Erreur: erreur}
	if domaine, _, err := auth.DomaineEtOrigine(r.Host); err == nil && auth.PasskeyPossible(domaine) {
		var ids []string
		if liste, err := auth.ListerPasskeys(r.Context(), c.db, s.UtilisateurID); err == nil {
			for _, p := range liste {
				if p.Domaine == domaine {
					ids = append(ids, p.IdentifiantB64())
				}
			}
		}
		if len(ids) > 0 {
			if defi, err := auth.NouveauDefiPasskey(); err == nil {
				t := c.maintenant()
				c.sessions.Modifier(s.Empreinte, func(x *auth.Session) { x.DefiPasskey, x.DefiPasskeyLe = defi, t })
				d.PKDefi, d.PKDomaine, d.PKIds = defi, domaine, strings.Join(ids, ",")
			}
		}
	}
	c.pages.rendreSeuleCode(w, code, "code", "Code 2FA", d)
}

// debutEtapeCode : contrôles communs aux trois façons de passer l'étape 2FA
// (code TOTP, code de secours, passkey) : session partielle, verrous, essai
// réservé, compte relu. ok = false : la réponse est déjà envoyée.
func (c *Console) debutEtapeCode(w http.ResponseWriter, r *http.Request) (s *auth.Session, u auth.Utilisateur, finir func(), ok bool) {
	s = c.sessionEtape(w, r, auth.EtapeCode)
	if s == nil {
		return nil, u, nil, false
	}
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	if c.verrous.BloqueAdresse(ip, t) || c.verrous.BloqueCompte(s.Nom, ip, t) {
		c.sessions.Supprimer(s.Empreinte)
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return nil, u, nil, false
	}
	// Essai réservé avant la vérification (rafale de codes, même répartie
	// sur plusieurs sessions partielles : voir verrou.go).
	finir, libre := c.verrous.Debuter(s.Nom, ip, c.securite.Load().Echecs, t)
	if !libre {
		c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", s.Nom, "raison", "trop d'essais en même temps")
		c.rendreEtapeCode(w, r, http.StatusTooManyRequests, s, messageSimultanes)
		return nil, u, nil, false
	}
	u, err := auth.LireUtilisateurID(r.Context(), c.db, s.UtilisateurID)
	if err != nil || u.Desactive || u.TOTP == "" || u.Generation != s.Generation {
		finir()
		c.sessions.Supprimer(s.Empreinte)
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return nil, u, nil, false
	}
	return s, u, finir, true
}

// echecEtapeCode : essai manqué à l'étape 2FA (compteurs, message à
// l'administrateur, retour au début après 5 essais).
func (c *Console) echecEtapeCode(w http.ResponseWriter, r *http.Request, s *auth.Session, u auth.Utilisateur, raison, message string) {
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	c.echecConnexion(ip, u.Nom, raison, t)
	// Le mot de passe était bon : s'il ne s'agit pas d'une faute de
	// frappe, quelqu'un d'autre le connaît.
	c.prevenirSecurite("2fa|"+u.Nom, "⚠️ mirador : "+raison+" pour « "+u.Nom+" » après un mot de passe juste.\nAdresse : "+ip+
		"\nSi ce n'est pas vous : changer le mot de passe de ce compte.", t)
	essais := 0
	c.sessions.Modifier(s.Empreinte, func(x *auth.Session) { x.EssaisCode++; essais = x.EssaisCode })
	if essais >= auth.EssaisCodeMax {
		c.sessions.Supprimer(s.Empreinte)
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	c.rendreEtapeCode(w, r, http.StatusUnauthorized, s, message)
}

// postCode : code TOTP, ou code de secours (champ « secours ») si le
// téléphone est perdu.
func (c *Console) postCode(w http.ResponseWriter, r *http.Request) {
	s, u, finir, ok := c.debutEtapeCode(w, r)
	if !ok {
		return
	}
	defer finir()
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	if secours := r.PostFormValue("secours"); secours != "" {
		bon, err := auth.UtiliserCodeSecours(r.Context(), c.db, u, secours, ip, t)
		if err != nil {
			http.Error(w, "base indisponible", http.StatusServiceUnavailable)
			return
		}
		if !bon {
			c.echecEtapeCode(w, r, s, u, "code de secours faux", "code de secours faux ou déjà utilisé")
			return
		}
		restants, _ := auth.CodesSecoursRestants(r.Context(), c.db, u.ID)
		c.journal.Warn("console : connexion avec un code de secours", "ip", ip, "utilisateur", u.Nom, "restants", restants)
		c.prevenirSecurite("secours|"+u.Nom, "🛟 mirador : « "+u.Nom+" » s'est connecté avec un code de secours.\nAdresse : "+ip+
			"\nCodes restants : "+strconv.Itoa(restants)+".\nSi ce n'est pas vous : changer le mot de passe et recréer la 2FA.", t)
		c.reussir(w, r, s, u.Nom, ip, t)
		return
	}
	secret, err := c.dechiffrerTOTP(u.TOTP)
	if err != nil {
		c.journal.Error("console : secret 2FA illisible (clé des secrets changée ?)", "utilisateur", u.Nom)
		http.Error(w, "2FA illisible : voir le journal du serveur", http.StatusInternalServerError)
		return
	}
	pas, bon := auth.VerifierTOTP(secret, r.PostFormValue("code"), t, u.TOTPDernier)
	if bon {
		if bon, err = auth.NoterPasTOTP(r.Context(), c.db, u.ID, pas, t); err != nil {
			http.Error(w, "base indisponible", http.StatusServiceUnavailable)
			return
		}
	}
	if !bon {
		c.echecEtapeCode(w, r, s, u, "code 2FA faux", "code faux ou déjà utilisé")
		return
	}
	c.reussir(w, r, s, u.Nom, ip, t)
}

// postPasskey : la passkey remplace le code 2FA (SPEC § 20). Le défi vient
// de la session partielle et ne sert qu'une fois.
func (c *Console) postPasskey(w http.ResponseWriter, r *http.Request) {
	s, u, finir, ok := c.debutEtapeCode(w, r)
	if !ok {
		return
	}
	defer finir()
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	defi, _ := c.sessions.PrendreDefiPasskey(s.Empreinte, t)
	err := func() error {
		domaine, origine, err := auth.DomaineEtOrigine(r.Host)
		if err != nil || defi == "" {
			return auth.ErrPasskey
		}
		liste, err := auth.ListerPasskeys(r.Context(), c.db, u.ID)
		if err != nil {
			return err
		}
		for i := range liste {
			p := &liste[i]
			if p.IdentifiantB64() != r.PostFormValue("identifiant") {
				continue
			}
			compteur, err := auth.VerifierConnexion(p, r.PostFormValue("client"), r.PostFormValue("authentificateur"),
				r.PostFormValue("signature"), defi, domaine, origine)
			if err != nil {
				return err
			}
			return auth.NoterPasskeyUtilisee(r.Context(), c.db, p.ID, compteur, t)
		}
		return auth.ErrPasskey // clé inconnue pour ce compte
	}()
	if err != nil {
		if !errors.Is(err, auth.ErrPasskey) {
			http.Error(w, "base indisponible", http.StatusServiceUnavailable)
			return
		}
		c.journal.Warn("console : passkey refusée", "ip", ip, "utilisateur", u.Nom, "detail", err.Error())
		c.echecEtapeCode(w, r, s, u, "passkey refusée", "passkey refusée : réessayer, ou utiliser le code 2FA")
		return
	}
	c.reussir(w, r, s, u.Nom, ip, t)
}

// reussir : connexion complète, nouvel identifiant de session.
func (c *Console) reussir(w http.ResponseWriter, r *http.Request, s *auth.Session, nom, ip string, t time.Time) {
	nouvelle := !c.verrous.AdresseConnue(nom, ip, t)
	c.verrous.Reussite(nom, ip, t)
	c.oublierComptes()
	u, err := auth.LireUtilisateurID(r.Context(), c.db, s.UtilisateurID)
	if err != nil {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	id := c.sessions.Renouveler(s.Empreinte, t, func(x *auth.Session) {
		x.Etape, x.Creee, x.SecretPropose, x.EssaisCode, x.Generation = auth.EtapeComplet, t, "", 0, u.Generation
		x.DefiPasskey, x.NomPasskey = "", ""
	})
	if id == "" {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	c.journal.Info("console : connexion", "ip", ip, "utilisateur", nom)
	if nouvelle {
		// Adresses connues gardées en mémoire : la première connexion après
		// un redémarrage du serveur prévient aussi.
		c.prevenirSecurite("adresse|"+nom+"|"+auth.CleAdresse(ip), "🔑 mirador : connexion de « "+nom+" » depuis une nouvelle adresse : "+ip+
			"\nSi ce n'est pas vous : fermer la session (page Sécurité) et changer le mot de passe.", t)
	}
	auth.PoserCookie(w, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// pageCreer2FA : le compte n'a pas encore de 2FA (premier passage). Le
// secret proposé reste en mémoire dans la session jusqu'à ce qu'un code
// juste le confirme.
func (c *Console) pageCreer2FA(w http.ResponseWriter, r *http.Request) {
	s := c.sessionEtape(w, r, auth.EtapeCreer)
	if s == nil {
		return
	}
	secret := s.SecretPropose
	if secret == "" {
		var err error
		if secret, err = auth.NouveauSecretTOTP(); err != nil {
			http.Error(w, "aléa indisponible", http.StatusInternalServerError)
			return
		}
		c.sessions.Modifier(s.Empreinte, func(x *auth.Session) { x.SecretPropose = secret })
	}
	c.rendreCreer2FA(w, http.StatusOK, s, secret, "")
}

func (c *Console) rendreCreer2FA(w http.ResponseWriter, code int, s *auth.Session, secret, erreur string) {
	qr, _ := auth.ImageQR(auth.URITOTP(s.Nom, secret))
	c.pages.rendreSeuleCode(w, code, "creer2fa", "Créer la 2FA", donneesConnexion{Nom: s.Nom, Jeton: s.CSRF, QR: qr,
		Secret: grouper(secret), Erreur: erreur})
}

// grouper : secret lisible par groupes de 4 (saisie à la main).
func grouper(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (c *Console) postCreer2FA(w http.ResponseWriter, r *http.Request) {
	s := c.sessionEtape(w, r, auth.EtapeCreer)
	if s == nil {
		return
	}
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	if s.SecretPropose == "" {
		http.Redirect(w, r, "/connexion/2fa-creer", http.StatusSeeOther)
		return
	}
	// Le compte est relu : désactivé, mot de passe changé (génération) ou
	// 2FA déjà créée ailleurs entre-temps → la session ne vaut plus rien
	// (revue du 11/11 : « utilisateur-secours » lancé par une commande).
	if u, err := auth.LireUtilisateurID(r.Context(), c.db, s.UtilisateurID); err != nil || u.Desactive || u.Generation != s.Generation || u.TOTP != "" ||
		c.verrous.BloqueAdresse(ip, t) {
		c.sessions.Supprimer(s.Empreinte)
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	pas, ok := auth.VerifierTOTP(s.SecretPropose, r.PostFormValue("code"), t, 0)
	if !ok {
		c.echecConnexion(ip, s.Nom, "code faux à la création de la 2FA", t)
		essais := 0
		c.sessions.Modifier(s.Empreinte, func(x *auth.Session) { x.EssaisCode++; essais = x.EssaisCode })
		if essais >= auth.EssaisCodeMax {
			c.sessions.Supprimer(s.Empreinte)
			http.Redirect(w, r, "/connexion", http.StatusSeeOther)
			return
		}
		c.rendreCreer2FA(w, http.StatusUnauthorized, s, s.SecretPropose, "code faux : vérifier l'heure du téléphone")
		return
	}
	chiffre, err := secrets.Chiffrer(c.opt.Cle, usageTOTP, s.SecretPropose)
	if err != nil {
		http.Error(w, "chiffrement impossible", http.StatusInternalServerError)
		return
	}
	if err := auth.DefinirTOTP(r.Context(), c.db, s.Nom, chiffre, pas, "web:"+s.Nom, ip, t); err != nil {
		http.Error(w, "enregistrement impossible", http.StatusServiceUnavailable)
		return
	}
	c.sessions.FermerUtilisateur(s.UtilisateurID, s.Empreinte)
	c.reussir(w, r, s, s.Nom, ip, t)
}

func (c *Console) postDeconnexion(w http.ResponseWriter, r *http.Request) {
	if ck, _ := r.Cookie(auth.NomCookie); ck != nil {
		if s := c.sessions.Lire(ck.Value); s != nil {
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
				http.Error(w, "jeton de formulaire invalide", http.StatusForbidden)
				return
			}
			c.sessions.Supprimer(s.Empreinte)
		}
	}
	auth.EffacerCookie(w)
	http.Redirect(w, r, "/connexion", http.StatusSeeOther)
}

// --- Premier administrateur -------------------------------------------------------

func (c *Console) pagePremier(w http.ResponseWriter, r *http.Request) {
	if !c.premierAFaire() {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	c.pages.rendreSeule(w, "premier", "Premier administrateur", donneesConnexion{Premier: true})
}

func (c *Console) postPremier(w http.ResponseWriter, r *http.Request) {
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	nom := strings.TrimSpace(r.PostFormValue("nom"))
	d := donneesConnexion{Nom: nom, Premier: true}
	if c.verrous.BloqueAdresse(ip, t) {
		d.Erreur = "trop d'essais depuis cette adresse : réessayer plus tard"
		c.pages.rendreSeuleCode(w, http.StatusTooManyRequests, "premier", "Premier administrateur", d)
		return
	}
	c.muPremier.Lock()
	defer c.muPremier.Unlock()
	if c.jetonPremier == "" {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.PostFormValue("jeton"))), []byte(c.jetonPremier)) != 1 {
		c.echecConnexion(ip, "", "jeton du premier admin faux", t)
		d.Erreur = "jeton faux (il est dans le journal du serveur)"
		c.pages.rendreSeuleCode(w, http.StatusUnauthorized, "premier", "Premier administrateur", d)
		return
	}
	mdp := r.PostFormValue("mot_de_passe")
	if mdp != r.PostFormValue("mot_de_passe2") {
		d.Erreur = "les deux mots de passe sont différents"
		c.pages.rendreSeuleCode(w, http.StatusBadRequest, "premier", "Premier administrateur", d)
		return
	}
	if err := auth.CreerUtilisateur(r.Context(), c.db, nom, auth.RoleAdmin, mdp, "premier-admin", ip, t); err != nil {
		d.Erreur = err.Error()
		c.pages.rendreSeuleCode(w, http.StatusBadRequest, "premier", "Premier administrateur", d)
		return
	}
	c.jetonPremier = "" // usage unique
	c.journal.Info("console : premier administrateur créé", "ip", ip, "utilisateur", nom)
	http.Redirect(w, r, "/connexion", http.StatusSeeOther)
}
