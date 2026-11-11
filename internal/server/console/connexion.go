// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"crypto/subtle"
	"errors"
	"html/template"
	"net/http"
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
	if n, err := NombreUtilisateurs(context.Background(), c.db); err == nil && n > 0 {
		c.jetonPremier = ""
		return false
	}
	return true
}

// nomJournal : nom écrit dans le journal. Un texte qui n'a pas la forme
// d'un nom de compte n'est jamais écrit : c'est souvent un mot de passe
// tapé par erreur dans le champ « Nom » (le journal est lu par CrowdSec).
func nomJournal(nom string) string {
	if nom == "" || VerifierNom(nom) == nil {
		return nom
	}
	return "(invalide)"
}

// echecConnexion : compteurs de verrouillage + ligne de journal (CrowdSec).
func (c *Console) echecConnexion(ip, nom, raison string, t time.Time) {
	sec := c.securite.Load()
	c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", nomJournal(nom), "raison", raison)
	if c.verrous.echecAdresse(ip, sec.Echecs, time.Duration(sec.Verrou), t) {
		c.journal.Warn("console : adresse verrouillée", "ip", ip, "duree", time.Duration(sec.Verrou).String())
	}
	if nom != "" && c.verrous.echecCompte(nom, 3*sec.Echecs, time.Duration(sec.Verrou), t) {
		c.journal.Warn("console : compte verrouillé", "ip", ip, "utilisateur", nomJournal(nom), "duree", time.Duration(sec.Verrou).String())
		c.prevenirVerrou(nom, ip, t)
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
	if c.verrous.bloqueAdresse(ip, t) {
		d.Erreur = "trop d'essais depuis cette adresse : réessayer plus tard"
		c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", nomJournal(nom), "raison", "adresse verrouillée")
		c.pages.rendreSeuleCode(w, http.StatusTooManyRequests, "connexion", "Connexion", d)
		return
	}
	if c.verrous.bloqueCompte(nom, ip, t) {
		d.Erreur = "trop d'essais sur ce compte : réessayer plus tard"
		c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", nomJournal(nom), "raison", "compte verrouillé")
		c.pages.rendreSeuleCode(w, http.StatusTooManyRequests, "connexion", "Connexion", d)
		return
	}
	// Essai réservé avant le calcul : une rafale de requêtes simultanées
	// ne donne pas plus d'essais que le seuil (voir verrou.go).
	finir, ok := c.verrous.debuter(nom, ip, c.securite.Load().Echecs, t)
	if !ok {
		d.Erreur = messageSimultanes
		c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", nomJournal(nom), "raison", "trop d'essais en même temps")
		c.pages.rendreSeuleCode(w, http.StatusTooManyRequests, "connexion", "Connexion", d)
		return
	}
	defer finir()
	u, err := LireUtilisateur(r.Context(), c.db, nom)
	if err != nil && !errors.Is(err, ErrUtilisateurInconnu) {
		http.Error(w, "base indisponible", http.StatusServiceUnavailable)
		return
	}
	empreinte := u.MotDePasse
	if u.ID == 0 {
		empreinte = empreinteLeurre // même durée de calcul qu'un vrai compte
	}
	bon, occupe := ComparerOccupe(empreinte, mdp)
	if occupe {
		d.Erreur = ErrOccupe.Error()
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
	etape := etapeCode
	if u.TOTP == "" {
		etape = etapeCreer
	}
	// Nouvelle session à chaque connexion : une ancienne session (même
	// partielle) n'est jamais reprise.
	if ck, _ := r.Cookie(NomCookie); ck != nil {
		if s := c.sessions.lire(ck.Value); s != nil {
			c.sessions.supprimer(s.empreinte)
		}
	}
	id := c.sessions.creer(&Session{UtilisateurID: u.ID, Nom: u.Nom, Generation: u.Generation, Etape: etape, Adresse: ip}, t)
	poserCookie(w, id)
	http.Redirect(w, r, cheminEtape(etape), http.StatusSeeOther)
}

// sessionEtape : session partielle à l'étape attendue, sinon retour au début.
func (c *Console) sessionEtape(w http.ResponseWriter, r *http.Request, etape string) *Session {
	ck, _ := r.Cookie(NomCookie)
	if ck == nil {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return nil
	}
	s := c.sessions.lire(ck.Value)
	if s == nil || s.Etape != etape || c.maintenant().Sub(s.Creee) > dureeAvantCode {
		if s != nil {
			c.sessions.supprimer(s.empreinte)
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
	s := c.sessionEtape(w, r, etapeCode)
	if s == nil {
		return
	}
	c.pages.rendreSeule(w, "code", "Code 2FA", donneesConnexion{Nom: s.Nom, Jeton: s.CSRF})
}

func (c *Console) postCode(w http.ResponseWriter, r *http.Request) {
	s := c.sessionEtape(w, r, etapeCode)
	if s == nil {
		return
	}
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	d := donneesConnexion{Nom: s.Nom, Jeton: s.CSRF}
	if c.verrous.bloqueAdresse(ip, t) || c.verrous.bloqueCompte(s.Nom, ip, t) {
		c.sessions.supprimer(s.empreinte)
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	// Essai réservé avant la vérification (rafale de codes, même répartie
	// sur plusieurs sessions partielles : voir verrou.go).
	finir, ok := c.verrous.debuter(s.Nom, ip, c.securite.Load().Echecs, t)
	if !ok {
		d.Erreur = messageSimultanes
		c.journal.Warn("console : échec de connexion", "ip", ip, "utilisateur", s.Nom, "raison", "trop d'essais en même temps")
		c.pages.rendreSeuleCode(w, http.StatusTooManyRequests, "code", "Code 2FA", d)
		return
	}
	defer finir()
	u, err := LireUtilisateurID(r.Context(), c.db, s.UtilisateurID)
	if err != nil || u.Desactive || u.TOTP == "" || u.Generation != s.Generation {
		c.sessions.supprimer(s.empreinte)
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	secret, err := c.dechiffrerTOTP(u.TOTP)
	if err != nil {
		c.journal.Error("console : secret 2FA illisible (clé des secrets changée ?)", "utilisateur", u.Nom)
		http.Error(w, "2FA illisible : voir le journal du serveur", http.StatusInternalServerError)
		return
	}
	pas, ok := VerifierTOTP(secret, r.PostFormValue("code"), t, u.TOTPDernier)
	if ok {
		if ok, err = NoterPasTOTP(r.Context(), c.db, u.ID, pas, t); err != nil {
			http.Error(w, "base indisponible", http.StatusServiceUnavailable)
			return
		}
	}
	if !ok {
		c.echecConnexion(ip, u.Nom, "code 2FA faux", t)
		// Le mot de passe était bon : s'il ne s'agit pas d'une faute de
		// frappe, quelqu'un d'autre le connaît.
		c.prevenirSecurite("2fa|"+u.Nom, "⚠️ mirador : code 2FA faux pour « "+u.Nom+" » après un mot de passe juste.\nAdresse : "+ip+
			"\nSi ce n'est pas vous : changer le mot de passe de ce compte.", t)
		essais := 0
		c.sessions.modifier(s.empreinte, func(x *Session) { x.EssaisCode++; essais = x.EssaisCode })
		if essais >= essaisCodeMax {
			c.sessions.supprimer(s.empreinte)
			http.Redirect(w, r, "/connexion", http.StatusSeeOther)
			return
		}
		d.Erreur = "code faux ou déjà utilisé"
		c.pages.rendreSeuleCode(w, http.StatusUnauthorized, "code", "Code 2FA", d)
		return
	}
	c.reussir(w, r, s, u.Nom, ip, t)
}

// reussir : connexion complète, nouvel identifiant de session.
func (c *Console) reussir(w http.ResponseWriter, r *http.Request, s *Session, nom, ip string, t time.Time) {
	nouvelle := !c.verrous.adresseConnue(nom, ip, t)
	c.verrous.reussite(nom, ip, t)
	c.oublierComptes()
	u, err := LireUtilisateurID(r.Context(), c.db, s.UtilisateurID)
	if err != nil {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	id := c.sessions.renouveler(s.empreinte, t, func(x *Session) {
		x.Etape, x.Creee, x.SecretPropose, x.EssaisCode, x.Generation = etapeComplet, t, "", 0, u.Generation
	})
	if id == "" {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	c.journal.Info("console : connexion", "ip", ip, "utilisateur", nom)
	if nouvelle {
		// Adresses connues gardées en mémoire : la première connexion après
		// un redémarrage du serveur prévient aussi.
		c.prevenirSecurite("adresse|"+nom+"|"+cleAdresse(ip), "🔑 mirador : connexion de « "+nom+" » depuis une nouvelle adresse : "+ip+
			"\nSi ce n'est pas vous : fermer la session (page Sécurité) et changer le mot de passe.", t)
	}
	poserCookie(w, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// pageCreer2FA : le compte n'a pas encore de 2FA (premier passage). Le
// secret proposé reste en mémoire dans la session jusqu'à ce qu'un code
// juste le confirme.
func (c *Console) pageCreer2FA(w http.ResponseWriter, r *http.Request) {
	s := c.sessionEtape(w, r, etapeCreer)
	if s == nil {
		return
	}
	secret := s.SecretPropose
	if secret == "" {
		var err error
		if secret, err = NouveauSecretTOTP(); err != nil {
			http.Error(w, "aléa indisponible", http.StatusInternalServerError)
			return
		}
		c.sessions.modifier(s.empreinte, func(x *Session) { x.SecretPropose = secret })
	}
	c.rendreCreer2FA(w, http.StatusOK, s, secret, "")
}

func (c *Console) rendreCreer2FA(w http.ResponseWriter, code int, s *Session, secret, erreur string) {
	qr, _ := ImageQR(URITOTP(s.Nom, secret))
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
	s := c.sessionEtape(w, r, etapeCreer)
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
	if u, err := LireUtilisateurID(r.Context(), c.db, s.UtilisateurID); err != nil || u.Desactive || u.Generation != s.Generation || u.TOTP != "" ||
		c.verrous.bloqueAdresse(ip, t) {
		c.sessions.supprimer(s.empreinte)
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	pas, ok := VerifierTOTP(s.SecretPropose, r.PostFormValue("code"), t, 0)
	if !ok {
		c.echecConnexion(ip, s.Nom, "code faux à la création de la 2FA", t)
		essais := 0
		c.sessions.modifier(s.empreinte, func(x *Session) { x.EssaisCode++; essais = x.EssaisCode })
		if essais >= essaisCodeMax {
			c.sessions.supprimer(s.empreinte)
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
	if err := DefinirTOTP(r.Context(), c.db, s.Nom, chiffre, pas, "web:"+s.Nom, ip, t); err != nil {
		http.Error(w, "enregistrement impossible", http.StatusServiceUnavailable)
		return
	}
	c.sessions.fermerUtilisateur(s.UtilisateurID, s.empreinte)
	c.reussir(w, r, s, s.Nom, ip, t)
}

func (c *Console) postDeconnexion(w http.ResponseWriter, r *http.Request) {
	if ck, _ := r.Cookie(NomCookie); ck != nil {
		if s := c.sessions.lire(ck.Value); s != nil {
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(s.CSRF)) != 1 {
				http.Error(w, "jeton de formulaire invalide", http.StatusForbidden)
				return
			}
			c.sessions.supprimer(s.empreinte)
		}
	}
	effacerCookie(w)
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
	if c.verrous.bloqueAdresse(ip, t) {
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
	if err := CreerUtilisateur(r.Context(), c.db, nom, RoleAdmin, mdp, "premier-admin", ip, t); err != nil {
		d.Erreur = err.Error()
		c.pages.rendreSeuleCode(w, http.StatusBadRequest, "premier", "Premier administrateur", d)
		return
	}
	c.jetonPremier = "" // usage unique
	c.journal.Info("console : premier administrateur créé", "ip", ip, "utilisateur", nom)
	http.Redirect(w, r, "/connexion", http.StatusSeeOther)
}
