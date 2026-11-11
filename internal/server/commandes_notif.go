// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/notif"
)

// Commandes de la sourdine et des notifications (en attendant l'interface).

// AideNotif : ajouté à AideCommandes.
const AideNotif = `
Sourdine et notifications (heures locales du fuseau des notifications, Europe/Paris par défaut) :
  silence [-pendant 2h] [-debut "AAAA-MM-JJ HH:MM"] [-commentaire "…"] 'SÉLECTEUR'
                                     ex. '{instance="pve1"}', '{alertname="CpuEleve",instance=~"vm-.*"}'
  silences | silence-annuler N
  maintenance [-debut "AAAA-MM-JJ HH:MM"] [-pendant 2h] [-jours lun,dim -heure 03:00] [-commentaire "…"] CIBLE
                                     CIBLE : machine:NOM, groupe:NOM ou 'SÉLECTEUR' ; avec -jours : récurrente
  maintenances | maintenance-supprimer N
  inhibition [-egales instance] [-commentaire "…"] 'SOURCE' 'CIBLE'
  inhibitions | inhibition-supprimer N
  dependance [-commentaire "…"] ENFANT PARENT
                                     ENFANT : machine:NOM, groupe:NOM ou 'SÉLECTEUR' ; PARENT : nom de machine
  dependances | dependance-supprimer N
  acquitter [-commentaire "…"] N     accusé de réception (numéro : commande alertes)
  envois [-limite 50]                historique des envois (et de ce qui serait parti en mode ombre)
  notif                              réglages des notifications (JSON)
  notif-reglages FICHIER|-           change les réglages (JSON complet, voir « notif ») ; secrets en clair
                                     chiffrés à l'enregistrement, « *** » = garder le secret actuel
  notif-tester CANAL                 message de test (telegram, ntfy, email), envoyé par le serveur en marche
  notif-etat                         état des canaux, file d'attente, heartbeat (serveur en marche)
  mode ombre|actif                   ombre : tout est calculé et noté, rien n'est envoyé`

// optionsNotif : options des commandes de notification.
type optionsNotif struct {
	pendant                   time.Duration
	debut, jours, heure, egal string
	commentaire               string
	limite                    int
}

func commandeNotif(ctx context.Context, db *sql.DB, donnees string, cle func() ([]byte, error), nom string, pos []string,
	o optionsNotif, sortie io.Writer, t time.Time) (traite, silencieuse bool, err error) {
	besoin := func(n int) error {
		if len(pos) != n {
			return fmt.Errorf("%d argument(s) attendu(s) ; voir « aide »", n)
		}
		return nil
	}
	numero := func() (int64, error) {
		if err := besoin(1); err != nil {
			return 0, err
		}
		return strconv.ParseInt(pos[0], 10, 64)
	}
	r, err := notif.LireReglages(ctx, db)
	if err != nil {
		return true, false, err
	}
	loc, _ := time.LoadLocation(r.Fuseau)
	debut := t
	if o.debut != "" {
		if debut, err = time.ParseInLocation("2006-01-02 15:04", o.debut, loc); err != nil {
			return true, false, fmt.Errorf("-debut %q : AAAA-MM-JJ HH:MM attendu", o.debut)
		}
	}
	afficherID := func(quoi string, id int64, err error) (bool, bool, error) {
		if err == nil {
			fmt.Fprintf(sortie, "%s n° %d créé(e)\n", quoi, id)
		}
		return true, true, err
	}
	switch nom {
	case "silence":
		if err := besoin(1); err != nil {
			return true, false, err
		}
		id, err := notif.AjouterSilence(ctx, db, pos[0], debut, o.pendant, qui, o.commentaire, t)
		return afficherID("silence", id, err)
	case "silences":
		return true, true, listerSilences(ctx, db, loc, sortie, t)
	case "silence-annuler":
		id, err := numero()
		if err == nil {
			err = notif.AnnulerSilence(ctx, db, id, qui, t)
		}
		return true, false, err
	case "maintenance":
		if err := besoin(1); err != nil {
			return true, false, err
		}
		var rec *notif.Recurrence
		if o.jours != "" {
			rec = &notif.Recurrence{Jours: strings.Split(o.jours, ","), Heure: o.heure, Duree: alertes.Duree(o.pendant), Fuseau: r.Fuseau}
		}
		id, err := notif.AjouterMaintenance(ctx, db, pos[0], debut, debut.Add(o.pendant), rec, qui, o.commentaire, t)
		return afficherID("maintenance", id, err)
	case "maintenances":
		return true, true, listerMaintenances(ctx, db, loc, sortie, t)
	case "inhibition":
		if err := besoin(2); err != nil {
			return true, false, err
		}
		var egales []string
		if o.egal != "" {
			egales = strings.Split(o.egal, ",")
		}
		id, err := notif.AjouterInhibition(ctx, db, pos[0], pos[1], egales, qui, o.commentaire, t)
		return afficherID("inhibition", id, err)
	case "inhibitions":
		ins, err := notif.ListerInhibitions(ctx, db)
		if err == nil {
			tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "N°\tSOURCE\tCIBLE\tÉGALES\tCOMMENTAIRE")
			for _, in := range ins {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", in.ID, in.Source, in.Cible, vide(strings.Join(in.Egales, ",")), vide(in.Commentaire))
			}
			err = tw.Flush()
		}
		return true, true, err
	case "dependance":
		if err := besoin(2); err != nil {
			return true, false, err
		}
		id, err := notif.AjouterDependance(ctx, db, pos[0], pos[1], qui, o.commentaire, t)
		return afficherID("dépendance", id, err)
	case "dependances":
		ds, err := notif.ListerDependances(ctx, db)
		if err == nil {
			tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "N°\tENFANT\tPARENT\tCOMMENTAIRE")
			for _, d := range ds {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", d.ID, d.Enfant, d.Parent, vide(d.Commentaire))
			}
			err = tw.Flush()
		}
		return true, true, err
	case "maintenance-supprimer", "inhibition-supprimer", "dependance-supprimer":
		id, err := numero()
		if err == nil {
			table := map[string]string{"maintenance-supprimer": "maintenances", "inhibition-supprimer": "inhibitions",
				"dependance-supprimer": "dependances"}[nom]
			err = notif.SupprimerElement(ctx, db, table, id, qui, t)
		}
		return true, false, err
	case "acquitter":
		id, err := numero()
		if err == nil {
			err = notif.Acquitter(ctx, db, id, qui, o.commentaire, t)
		}
		return true, false, err
	case "envois":
		return true, true, listerEnvois(ctx, db, o.limite, loc, sortie)
	case "notif":
		b, err := json.MarshalIndent(r.Masques(), "", "  ")
		if err == nil {
			fmt.Fprintln(sortie, string(b))
		}
		return true, true, err
	case "notif-reglages":
		if err := besoin(1); err != nil {
			return true, false, err
		}
		k, err := cle()
		if err != nil {
			return true, false, err
		}
		return true, false, reglerNotif(ctx, db, k, pos[0], t)
	case "notif-tester":
		if err := besoin(1); err != nil {
			return true, false, err
		}
		return true, true, testerCanal(ctx, db, pos[0], sortie, t)
	case "notif-etat":
		return true, true, demanderAdmin(donnees, "/notif", nil, sortie)
	case "mode":
		if err := besoin(1); err != nil {
			return true, false, err
		}
		if pos[0] != notif.Ombre && pos[0] != notif.Actif {
			return true, false, errors.New("mode : ombre ou actif")
		}
		r.Mode = pos[0]
		k, err := cle()
		if err != nil {
			return true, false, err
		}
		return true, false, notif.EcrireReglages(ctx, db, r, k, qui, t)
	}
	return false, false, nil
}

func reglerNotif(ctx context.Context, db *sql.DB, cle []byte, fichier string, t time.Time) error {
	var brut []byte
	var err error
	if fichier == "-" {
		brut, err = io.ReadAll(io.LimitReader(entree, 256<<10))
	} else {
		brut, err = os.ReadFile(fichier) // #nosec G304 -- fichier donné par l'administrateur
	}
	if err != nil {
		return err
	}
	r := notif.ReglagesDefaut()
	dec := json.NewDecoder(strings.NewReader(string(brut)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf("réglages illisibles : %w", err)
	}
	return notif.EcrireReglages(ctx, db, r, cle, qui, t)
}

// testerCanal : la demande est faite par le serveur en marche (lui seul a les
// secrets déchiffrés) ; on attend son résultat 30 s au plus.
func testerCanal(ctx context.Context, db *sql.DB, canal string, sortie io.Writer, t time.Time) error {
	id, err := notif.DemanderTest(ctx, db, canal, t)
	if err != nil {
		return err
	}
	for fin := time.Now().Add(30 * time.Second); time.Now().Before(fin); time.Sleep(500 * time.Millisecond) {
		statut, e, err := notif.ResultatTest(ctx, db, id)
		if err != nil {
			return err
		}
		switch statut {
		case "ok":
			fmt.Fprintf(sortie, "message de test envoyé par %s\n", canal)
			return nil
		case "echec":
			return fmt.Errorf("échec : %s", e)
		}
	}
	return errors.New("pas de réponse du serveur en 30 s (est-il démarré ?)")
}

func listerSilences(ctx context.Context, db *sql.DB, loc *time.Location, sortie io.Writer, t time.Time) error {
	ss, err := notif.ListerSilences(ctx, db, true, t)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "N°\tÉTAT\tDÉBUT\tFIN\tSÉLECTEUR\tAUTEUR\tCOMMENTAIRE")
	for _, x := range ss {
		etat := "en cours"
		switch {
		case !t.Before(x.Fin):
			etat = "fini"
		case t.Before(x.Debut):
			etat = "à venir"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", x.ID, etat, x.Debut.In(loc).Format("02/01 15:04"),
			x.Fin.In(loc).Format("02/01 15:04"), x.Selecteur, x.Auteur, vide(x.Commentaire))
	}
	return tw.Flush()
}

func listerMaintenances(ctx context.Context, db *sql.DB, loc *time.Location, sortie io.Writer, t time.Time) error {
	ms, err := notif.ListerMaintenances(ctx, db)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "N°\tCIBLE\tQUAND\tAUTEUR\tCOMMENTAIRE")
	for _, m := range ms {
		quand := m.Debut.In(loc).Format("02/01 15:04") + " → " + m.Fin.In(loc).Format("02/01 15:04")
		if m.Recurrence != nil {
			quand = fmt.Sprintf("chaque %s à %s pendant %s", strings.Join(m.Recurrence.Jours, ","), m.Recurrence.Heure, m.Recurrence.Duree)
		} else if !t.Before(m.Fin) {
			quand += " (finie)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", m.ID, m.Cible, quand, m.Auteur, vide(m.Commentaire))
	}
	return tw.Flush()
}

func listerEnvois(ctx context.Context, db *sql.DB, limite int, loc *time.Location, sortie io.Writer) error {
	if limite < 1 || limite > 10000 {
		return errors.New("limite entre 1 et 10 000")
	}
	es, err := notif.ListerEnvois(ctx, db, limite)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "QUAND\tCANAL\tSTATUT\tTYPE\tGRAVITÉ\tNB\tDURÉE\tGROUPE\tERREUR")
	for _, e := range es {
		canal := e.Canal
		if e.Secours {
			canal += " (secours)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", e.Le.In(loc).Format("02/01 15:04:05"), canal, e.Statut, e.Type,
			e.Gravite, e.Nb, e.Duree, e.Groupe, vide(e.Erreur))
	}
	return tw.Flush()
}
