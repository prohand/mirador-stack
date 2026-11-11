// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/sauvegarde"
)

// AideCommandes : commandes d'administration, en attendant l'interface web
// (étape 5). Dans le conteneur :
//
//	docker exec mirador-server /mirador-server -config /etc/mirador/server.conf COMMANDE …
const AideCommandes = `Commandes (les options se placent AVANT le nom de la machine) :
  machines                           liste des machines
  jeton [-duree 24h] [-site S] [-env T] [-groupe G]  jeton d'enrôlement à usage unique (affiché une seule fois) ; site, type et groupe de la future machine
  approuver [-sans-ip] NOM           approuve une machine en attente (adresse de la demande
                                     gardée comme seule adresse autorisée, sauf -sans-ip)
  refuser NOM                        refuse une machine (ses tokens sont révoqués)
  revoquer NOM                       révoque les tokens d'une machine
  token NOM                          token pour une installation sans enrôlement
                                     (crée la machine si besoin ; affiché une seule fois)
  ips NOM "LISTE"                    adresses/préfixes autorisés pour le token ("" = toutes)
  etiquettes [-site S] [-env E] [-groupe G] NOM
  config PORTEE FICHIER|-            configuration des agents (defaut, groupe:NOM, machine:NOM) ;
                                     fichier vide ou {} = retirer
  voir-config NOM                    configuration envoyée à une machine
  publier MANIFESTE BINAIRE|-        publie une version signée par mirador-release
                                     (MANIFESTE : fichier ou texte JSON ; « - » : binaire lu
                                     sur l'entrée standard)
  cible [-machine NOM] VERSION       version assignée (« aucune » pour retirer)

Consultation (serveur en marche) :
  voir-metriques [-machine NOM] [-depuis 1h] METRIQUE
                                     séries, nombre de points, dernière valeur
  voir-logs [-machine NOM] [-niveau err] [-unite U] [-mots "…"] [-contient "…"] [-depuis 1h] [-limite 50]
                                     dernières lignes de logs

Sauvegardes :
  sauvegarder [-metriques] [-logs]   sauvegarde maintenant (faite par le serveur en marche)
  sauvegardes                        historique, archives locales, réglages
  sauvegarde-reglages FICHIER|-      réglages (JSON ; secrets en clair, chiffrés à l'enregistrement ;
                                     « *** » = garder le secret actuel)
  verifier-sauvegarde ARCHIVE|-      déchiffre et contrôle une archive sans rien restaurer
  restaurer ARCHIVE|-                restaure dans un dossier de données VIDE (serveur arrêté)

Effet sur le serveur en marche : immédiat, sauf une révocation (60 s au plus).` + AideAlertes + AideNotif + AideConsole

// qui : auteur noté dans l'audit pour une commande.
const qui = "commande"

// entree : entrée standard (remplaçable dans les tests).
var entree io.Reader = os.Stdin

// Commande exécute une commande d'administration. Elle ouvre la base
// directement (SQLite accepte un second processus) ; elle ne migre jamais.
func Commande(ctx context.Context, cfg config.Serveur, args []string, sortie, erreurs io.Writer) int {
	if len(args) == 0 || args[0] == "aide" {
		fmt.Fprintln(sortie, AideCommandes)
		return 0
	}
	// Sans la base : restauration (dossier vide) et vérification d'archive.
	if args[0] == "restaurer" || args[0] == "verifier-sauvegarde" {
		return commandeArchive(cfg, args, sortie, erreurs)
	}
	db, err := base.Ouvrir(filepath.Join(cfg.Donnees, "mirador.db"))
	if err != nil {
		fmt.Fprintf(erreurs, "base : %v\n", err)
		return 1
	}
	defer db.Close()
	if v, err := base.Version(ctx, db); err != nil || v != base.VersionAttendue() {
		fmt.Fprintf(erreurs, "base en version %d, attendue %d : démarrez d'abord le serveur (il migre la base)\n", v, base.VersionAttendue())
		return 1
	}
	maintenant := time.Now()
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(erreurs)
	duree := fs.Duration("duree", 24*time.Hour, "validité du jeton")
	sansIP := fs.Bool("sans-ip", false, "ne pas limiter le token à l'adresse de la demande")
	site := fs.String("site", "", "site")
	env := fs.String("env", "", "environnement")
	groupe := fs.String("groupe", "", "groupe (portée de configuration)")
	machine := fs.String("machine", "", "machine")
	avecMetriques := fs.Bool("metriques", false, "inclure les métriques")
	avecLogs := fs.Bool("logs", false, "inclure les logs")
	depuis := fs.String("depuis", "1h", "période (ex. 30m, 6h, 168h)")
	niveau := fs.String("niveau", "", "niveau des logs (err, warning…)")
	unite := fs.String("unite", "", "unité systemd")
	mots := fs.String("mots", "", "mots entiers, tous présents")
	contient := fs.String("contient", "", "morceau de texte")
	limite := fs.Int("limite", 50, "lignes de logs, alertes")
	essai := fs.Bool("essai", false, "rapport seul, rien n'est enregistré")
	desactivees := fs.Bool("desactivees", false, "règles importées désactivées")
	enCSV := fs.Bool("csv", false, "sortie CSV")
	var on optionsNotif
	fs.DurationVar(&on.pendant, "pendant", 2*time.Hour, "durée d'un silence ou d'une maintenance")
	fs.StringVar(&on.debut, "debut", "", "début (AAAA-MM-JJ HH:MM, heure locale)")
	fs.StringVar(&on.jours, "jours", "", "maintenance récurrente : jours (lun,mar,…,dim)")
	fs.StringVar(&on.heure, "heure", "", "maintenance récurrente : heure de début (HH:MM)")
	fs.StringVar(&on.egal, "egales", "", "inhibition : étiquettes égales (ex. instance)")
	fs.StringVar(&on.commentaire, "commentaire", "", "commentaire")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	pos := fs.Args()
	besoin := func(n int) error {
		if len(pos) != n {
			return fmt.Errorf("%d argument(s) attendu(s) ; voir « aide »", n)
		}
		return nil
	}

	switch args[0] {
	case "machines":
		err = listerMachines(ctx, db, sortie, maintenant)
	case "jeton":
		var j string
		if j, err = flotte.CreerJetonEtiquettes(ctx, db, *duree, flotte.Etiquettes{Site: *site, Env: *env, Groupe: *groupe}, qui, maintenant); err == nil {
			fmt.Fprintf(sortie, "Jeton d'enrôlement (usage unique, valable jusqu'au %s) :\n%s\n",
				maintenant.Add(*duree).Format("02/01/2006 15:04"), j)
			fmt.Fprintln(sortie, "Il ne sera plus affiché. À passer au script d'installation de l'agent.")
		}
	case "approuver":
		if err = besoin(1); err == nil {
			err = flotte.Approuver(ctx, db, pos[0], !*sansIP, qui, maintenant)
		}
	case "refuser":
		if err = besoin(1); err == nil {
			err = flotte.Refuser(ctx, db, pos[0], qui, maintenant)
		}
	case "revoquer":
		if err = besoin(1); err == nil {
			err = flotte.Revoquer(ctx, db, pos[0], qui, maintenant)
		}
	case "token":
		if err = besoin(1); err == nil {
			var t string
			if t, err = flotte.TokenManuel(ctx, db, pos[0], qui, maintenant); err == nil {
				fmt.Fprintf(sortie, "Token de %s (les anciens sont révoqués) :\n%s\n", pos[0], t)
				fmt.Fprintln(sortie, "Il ne sera plus affiché. À écrire dans /etc/mirador-agent/token (0640).")
			}
		}
	case "ips":
		if err = besoin(2); err == nil {
			err = flotte.DefinirIPs(ctx, db, pos[0], pos[1], qui, maintenant)
		}
	case "etiquettes":
		if err = besoin(1); err == nil {
			err = flotte.DefinirEtiquettes(ctx, db, pos[0], *site, *env, *groupe, qui, maintenant)
		}
	case "config":
		if err = besoin(2); err == nil {
			err = definirConfig(ctx, db, pos[0], pos[1], maintenant)
		}
	case "voir-config":
		if err = besoin(1); err == nil {
			var id int64
			if id, err = flotte.IDMachine(ctx, db, pos[0]); err == nil {
				var doc []byte
				if doc, _, err = flotte.ConfigMachine(ctx, db, id); err == nil {
					fmt.Fprintln(sortie, string(doc))
				}
			}
		}
	case "publier":
		if err = besoin(2); err == nil {
			err = publier(ctx, db, filepath.Join(cfg.Donnees, "versions"), pos[0], pos[1], maintenant)
		}
	case "cible":
		if err = besoin(1); err == nil {
			version := pos[0]
			if version == "aucune" {
				version = ""
			}
			var id int64
			if *machine != "" {
				id, err = flotte.IDMachine(ctx, db, *machine)
			}
			if err == nil {
				err = flotte.DefinirCible(ctx, db, id, version, qui, maintenant)
			}
		}
	case "voir-metriques":
		if err = besoin(1); err == nil {
			err = demanderAdmin(cfg.Donnees, "/metriques", url.Values{"nom": {pos[0]}, "machine": {*machine}, "depuis": {*depuis}}, sortie)
		}
	case "voir-logs":
		if err = besoin(0); err == nil {
			err = demanderAdmin(cfg.Donnees, "/logs", url.Values{"host": {*machine}, "level": {*niveau}, "unit": {*unite},
				"mots": {*mots}, "contient": {*contient}, "depuis": {*depuis}, "limite": {strconv.Itoa(*limite)}}, sortie)
		}
	case "sauvegarder":
		err = sauvegarder(ctx, db, *avecMetriques, *avecLogs, sortie)
	case "sauvegardes":
		err = afficherSauvegardes(ctx, db, cfg, sortie, maintenant)
	case "sauvegarde-reglages":
		if err = besoin(1); err == nil {
			err = reglerSauvegardes(ctx, db, cfg, pos[0], maintenant)
		}
	default:
		traite, silencieuse, e := commandeAlertes(ctx, db, cfg.Donnees, args[0], pos, *essai, *desactivees, *enCSV, *limite, sortie, maintenant)
		if !traite {
			traite, e = commandeConsole(ctx, db, args[0], pos, sortie, maintenant)
		}
		if !traite {
			on.limite = *limite
			lireCle := func() ([]byte, error) { return config.LireCle(cfg.FichierCle) }
			traite, silencieuse, e = commandeNotif(ctx, db, cfg.Donnees, lireCle, args[0], pos, on, sortie, maintenant)
		}
		switch {
		case !traite:
			err = fmt.Errorf("commande inconnue %q ; voir « aide »", args[0])
		case e != nil:
			err = e
		case silencieuse:
			return 0
		}
	}
	if err != nil {
		fmt.Fprintf(erreurs, "%s : %v\n", args[0], err)
		return 1
	}
	if !map[string]bool{"machines": true, "voir-config": true, "jeton": true, "token": true, "sauvegarder": true, "sauvegardes": true,
		"voir-metriques": true, "voir-logs": true}[args[0]] {
		fmt.Fprintln(sortie, "fait")
	}
	return 0
}

func listerMachines(ctx context.Context, db *sql.DB, sortie io.Writer, maintenant time.Time) error {
	ms, err := flotte.ListerMachines(ctx, db)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NOM\tSTATUT\tTYPE\tVERSION\tCIBLE\tVU IL Y A\tDERNIÈRE IP\tIP AUTORISÉES\tTOKENS\tMISE À JOUR")
	for _, m := range ms {
		maj := m.MajEssai
		if m.MajEchec != "" {
			maj = "échec : " + m.MajEchec
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", m.Nom, m.Statut, m.Type, vide(m.Version), vide(m.VersionCible),
			age(m.VuLe, maintenant), vide(m.DerniereIP), vide(m.IPsAutorisees), m.TokensActifs, vide(maj))
	}
	return tw.Flush()
}

func vide(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func age(t int64, maintenant time.Time) string {
	if t == 0 {
		return "jamais"
	}
	return maintenant.Sub(time.Unix(t, 0)).Round(time.Second).String()
}

// definirConfig : la portée « machine:NOM » est traduite en « machine:ID ».
func definirConfig(ctx context.Context, db *sql.DB, portee, fichier string, maintenant time.Time) error {
	if nom, ok := strings.CutPrefix(portee, "machine:"); ok {
		id, err := flotte.IDMachine(ctx, db, nom)
		if err != nil {
			return err
		}
		portee = "machine:" + strconv.FormatInt(id, 10)
	}
	var doc []byte
	var err error
	if fichier == "-" {
		doc, err = io.ReadAll(io.LimitReader(entree, 256<<10))
	} else {
		doc, err = os.ReadFile(fichier) // #nosec G304 -- fichier donné par l'administrateur
	}
	if err != nil {
		return err
	}
	return flotte.DefinirConfig(ctx, db, portee, doc, qui, maintenant)
}

func publier(ctx context.Context, db *sql.DB, dossier, manifeste, cheminBinaire string, maintenant time.Time) error {
	// Le manifeste peut être donné en texte : pratique avec « docker exec »,
	// sans copier de fichier dans le conteneur (système en lecture seule).
	brut := []byte(manifeste)
	if !strings.HasPrefix(strings.TrimSpace(manifeste), "{") {
		var err error
		if brut, err = os.ReadFile(manifeste); err != nil { // #nosec G304 -- fichier donné par l'administrateur
			return err
		}
	}
	var m protocole.Manifeste
	dec := json.NewDecoder(bytes.NewReader(brut))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("manifeste illisible : %w", err)
	}
	var binaire []byte
	var err error
	if cheminBinaire == "-" {
		binaire, err = io.ReadAll(io.LimitReader(entree, majagent.TailleMax+1))
	} else {
		binaire, err = os.ReadFile(cheminBinaire) // #nosec G304 -- fichier donné par l'administrateur
	}
	if err != nil {
		return err
	}
	if int64(len(binaire)) != m.Taille {
		return errors.New("taille du binaire différente de celle du manifeste")
	}
	return flotte.Publier(ctx, db, dossier, &m, binaire, qui, maintenant)
}

// --- Sauvegardes -------------------------------------------------------------------

// Délais de la commande sauvegarder (modifiables dans les tests).
var (
	rythmeSuivi   = time.Second
	delaiServeur  = time.Minute
	dureeMaxSuivi = 2 * time.Hour
)

// sauvegarder dépose une demande ; le serveur en marche la fait (il est seul
// à pouvoir copier les métriques de façon cohérente) ; on attend le résultat.
func sauvegarder(ctx context.Context, db *sql.DB, metriques, logs bool, sortie io.Writer) error {
	id, err := sauvegarde.Demander(ctx, db, "commande", metriques, logs, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(sortie, "Sauvegarde n° %d demandée, en attente du serveur…\n", id)
	debut := time.Now()
	for time.Since(debut) < dureeMaxSuivi {
		e, err := sauvegarde.Etat(ctx, db, id)
		if err != nil {
			return err
		}
		switch e.Statut {
		case "ok":
			fmt.Fprintf(sortie, "Sauvegarde faite : %s (%s), copiée vers : %s\n", filepath.Base(e.Fichier), sauvegarde.FormatTaille(e.Taille), e.Destinations)
			return nil
		case "echec":
			return fmt.Errorf("sauvegarde en échec : %s", e.Erreur)
		case "demandee":
			if time.Since(debut) > delaiServeur {
				return errors.New("le serveur n'a pas pris la demande : est-il démarré ? (elle reste en attente et sera faite à son démarrage)")
			}
		}
		time.Sleep(rythmeSuivi)
	}
	return errors.New("sauvegarde toujours en cours : voir « sauvegardes »")
}

// AgeMaxSauvegarde : au-delà, « sauvegardes » signale un problème (l'alerte
// de l'étape 4 utilisera la même règle).
const AgeMaxSauvegarde = 26 * time.Hour

func afficherSauvegardes(ctx context.Context, db *sql.DB, cfg config.Serveur, sortie io.Writer, maintenant time.Time) error {
	r, err := sauvegarde.LireReglages(ctx, db)
	if err != nil {
		return err
	}
	masque, _ := json.MarshalIndent(r.Masques(), "", "  ")
	fmt.Fprintf(sortie, "Réglages :\n%s\n\n", masque)
	if d, ok, err := sauvegarde.DerniereReussie(ctx, db); err != nil {
		return err
	} else if !ok {
		fmt.Fprintln(sortie, "ATTENTION : aucune sauvegarde réussie.")
	} else if age := maintenant.Sub(time.Unix(d.Fin, 0)); age > AgeMaxSauvegarde {
		fmt.Fprintf(sortie, "ATTENTION : dernière sauvegarde réussie il y a %s.\n", age.Round(time.Minute))
	}
	h, err := sauvegarde.Historique(ctx, db, 20)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "N°\tDEMANDÉE\tORIGINE\tSTATUT\tTAILLE\tCONTENU\tDESTINATIONS\tERREUR")
	for _, e := range h {
		contenu := "configuration"
		if e.Metriques {
			contenu += "+métriques"
		}
		if e.Logs {
			contenu += "+logs"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, time.Unix(e.DemandeeLe, 0).UTC().Format("02/01 15:04"), e.Origine,
			e.Statut, sauvegarde.FormatTaille(e.Taille), contenu, vide(e.Destinations), vide(e.Erreur))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	archives, err := sauvegarde.Lister(sauvegarde.DossierArchives(cfg.Donnees))
	if err != nil {
		return err
	}
	fmt.Fprintf(sortie, "\nArchives locales (%s) :\n", sauvegarde.DossierArchives(cfg.Donnees))
	for _, a := range archives {
		fmt.Fprintf(sortie, "  %s  %s\n", a.Nom, sauvegarde.FormatTaille(a.Taille))
	}
	return nil
}

func reglerSauvegardes(ctx context.Context, db *sql.DB, cfg config.Serveur, fichier string, maintenant time.Time) error {
	var brut []byte
	var err error
	if fichier == "-" {
		brut, err = io.ReadAll(io.LimitReader(entree, 64<<10))
	} else {
		brut, err = os.ReadFile(fichier) // #nosec G304 -- fichier donné par l'administrateur
	}
	if err != nil {
		return err
	}
	var r sauvegarde.Reglages
	dec := json.NewDecoder(bytes.NewReader(brut))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf("réglages illisibles : %w", err)
	}
	cle, err := config.LireCle(cfg.FichierCle)
	if err != nil {
		return err
	}
	anciens, err := sauvegarde.LireReglages(ctx, db)
	if err != nil {
		return err
	}
	if r, err = sauvegarde.PreparerSecrets(r, anciens, cle); err != nil {
		return err
	}
	return sauvegarde.EcrireReglages(ctx, db, r, qui, maintenant)
}

// commandeArchive : restaurer et verifier-sauvegarde.
func commandeArchive(cfg config.Serveur, args []string, sortie, erreurs io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintf(erreurs, "%s : une archive (ou « - » pour l'entrée standard) attendue\n", args[0])
		return 2
	}
	cle, err := config.LireCle(cfg.FichierCle)
	if err != nil {
		fmt.Fprintf(erreurs, "clé des secrets : %v\n", err)
		return 1
	}
	var r io.Reader = entree
	if args[1] != "-" {
		f, err := os.Open(args[1]) // #nosec G304 -- archive donnée par l'administrateur
		if err != nil {
			fmt.Fprintln(erreurs, err)
			return 1
		}
		defer f.Close()
		r = f
	}
	var man *sauvegarde.Manifeste
	if args[0] == "verifier-sauvegarde" {
		man, err = sauvegarde.Verifier(r, cle, cfg.Donnees, sauvegarde.TailleMaxRestauration)
	} else {
		man, err = sauvegarde.Restaurer(r, cle, cfg.Donnees, sauvegarde.TailleMaxRestauration)
	}
	if err != nil {
		fmt.Fprintf(erreurs, "%s : %v\n", args[0], err)
		return 1
	}
	contenu := "configuration"
	if man.Metriques {
		contenu += ", métriques"
	}
	if man.Logs {
		contenu += ", logs"
	}
	fmt.Fprintf(sortie, "Archive valide : sauvegarde du %s (serveur %s, base v%d), %d fichiers, %s ; contenu : %s.\n",
		man.Date.Format("02/01/2006 15:04 UTC"), vide(man.Version), man.Schema, len(man.Fichiers), sauvegarde.FormatTaille(man.TailleBrut), contenu)
	if args[0] == "restaurer" {
		fmt.Fprintf(sortie, "Restaurée dans %s. Le fichier de démarrage d'origine est dans %s (pour comparaison).\n",
			cfg.Donnees, filepath.Join(cfg.Donnees, "restauration", "server.conf"))
		fmt.Fprintln(sortie, "Démarrez le serveur avec la MÊME clé des secrets ; il migrera la base si besoin.")
	}
	return 0
}
