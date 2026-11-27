// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
	"mirador-stack/internal/server/secrets"
)

// Export / import de toute la configuration en YAML (brief), format
// versionné. Secrets : masqués (« *** » : gardés tels quels à l'import) ou,
// au choix, chiffrés (lisibles seulement par un serveur qui a la même clé des
// secrets). Les machines sont désignées par leur nom (pas leur numéro), pour
// pouvoir importer sur un autre serveur.

// VersionExport : version du format.
const VersionExport = 1

type machineExport struct {
	Nom           string `json:"nom"`
	Site          string `json:"site,omitempty"`
	Env           string `json:"env,omitempty"`
	Groupe        string `json:"groupe,omitempty"`
	IPsAutorisees string `json:"ips_autorisees,omitempty"`
	VersionCible  string `json:"version_cible,omitempty"`
}

type sourdineExport struct {
	Inhibitions  []inhibitionExport  `json:"inhibitions"`
	Dependances  []dependanceExport  `json:"dependances"`
	Maintenances []maintenanceExport `json:"maintenances"`
}

type inhibitionExport struct {
	Source      string   `json:"source"`
	Cible       string   `json:"cible"`
	Egales      []string `json:"egales"`
	Commentaire string   `json:"commentaire,omitempty"`
}

type dependanceExport struct {
	Enfant      string `json:"enfant"`
	Parent      string `json:"parent"`
	Commentaire string `json:"commentaire,omitempty"`
}

type maintenanceExport struct {
	Cible       string            `json:"cible"`
	Debut       time.Time         `json:"debut"`
	Fin         time.Time         `json:"fin"`
	Recurrence  *notif.Recurrence `json:"recurrence,omitempty"`
	Commentaire string            `json:"commentaire,omitempty"`
}

// Configuration : tout ce qui se règle dans l'interface.
type Configuration struct {
	Version       int                       `json:"version"`
	ExporteLe     time.Time                 `json:"exporte_le"`
	Serveur       string                    `json:"serveur"`
	Regles        []alertes.Regle           `json:"regles"`
	Notifications notif.Reglages            `json:"notifications"`
	Vmalert       alertes.ReglagesVmalert   `json:"vmalert"`
	Securite      Securite                  `json:"securite"`
	Conservation  Conservation              `json:"conservation"`
	Interface     ReglagesInterface         `json:"interface"`
	Sauvegarde    sauvegarde.Reglages       `json:"sauvegarde"`
	FenetreMaj    flotte.Fenetre            `json:"fenetre_maj"`
	CibleAgents   string                    `json:"cible_agents,omitempty"`
	ConfigAgents  map[string]map[string]any `json:"config_agents"` // portée → document (machine:NOM)
	Machines      []machineExport           `json:"machines"`
	Sourdine      sourdineExport            `json:"sourdine"`
}

func (c *Console) configuration(ctx context.Context, avecSecrets bool, t time.Time) (Configuration, error) {
	cf := Configuration{Version: VersionExport, ExporteLe: t.UTC(), Serveur: c.opt.Version, ConfigAgents: map[string]map[string]any{}}
	var e [10]error
	cf.Regles, e[0] = alertes.ListerRegles(ctx, c.db)
	cf.Notifications, e[1] = notif.LireReglages(ctx, c.db)
	cf.Vmalert, e[2] = alertes.LireReglagesVmalert(ctx, c.db)
	cf.Securite, e[3] = LireSecurite(ctx, c.db)
	cf.Conservation, e[4] = LireConservation(ctx, c.db)
	cf.Interface = lireInterface(ctx, c.db)
	cf.Sauvegarde, e[5] = sauvegarde.LireReglages(ctx, c.db)
	cf.FenetreMaj, e[6] = flotte.LireFenetre(ctx, c.db)
	cf.CibleAgents, e[7] = flotte.CibleGlobale(ctx, c.db)
	if !avecSecrets {
		cf.Notifications = cf.Notifications.Masques()
		cf.Sauvegarde = cf.Sauvegarde.Masques()
	}
	ms, err := c.machines(ctx)
	e[8] = err
	noms := map[string]string{}
	for _, m := range ms {
		noms[strconv.FormatInt(m.ID, 10)] = m.Nom
		cf.Machines = append(cf.Machines, machineExport{Nom: m.Nom, Site: m.Site, Env: m.Env, Groupe: m.Groupe,
			IPsAutorisees: m.IPsAutorisees, VersionCible: m.VersionCible})
	}
	rows, err := c.db.QueryContext(ctx, `SELECT substr(cle, 14), valeur FROM reglages WHERE cle LIKE 'config\_agent:%' ESCAPE '\'`)
	if err == nil {
		for rows.Next() {
			var p, v string
			if rows.Scan(&p, &v) != nil {
				continue
			}
			if id, ok := strings.CutPrefix(p, "machine:"); ok {
				p = "machine:" + noms[id]
			}
			doc := map[string]any{}
			if json.Unmarshal([]byte(v), &doc) == nil {
				if !avecSecrets {
					secrets.Masquer(doc) // mots de passe SNMP / UniFi chiffrés → « *** »
				}
				cf.ConfigAgents[p] = doc
			}
		}
		err = rows.Err() // lecture coupée : export refusé plutôt qu'incomplet
		_ = rows.Close() // lecture seule : une erreur de fermeture est sans effet
	}
	e[9] = err
	inh, _ := notif.ListerInhibitions(ctx, c.db)
	for _, i := range inh {
		cf.Sourdine.Inhibitions = append(cf.Sourdine.Inhibitions, inhibitionExport{i.Source, i.Cible, i.Egales, i.Commentaire})
	}
	deps, _ := notif.ListerDependances(ctx, c.db)
	for _, d := range deps {
		cf.Sourdine.Dependances = append(cf.Sourdine.Dependances, dependanceExport{d.Enfant, d.Parent, d.Commentaire})
	}
	mts, _ := notif.ListerMaintenances(ctx, c.db)
	for _, m := range mts {
		if m.Recurrence != nil || m.Fin.After(t) {
			cf.Sourdine.Maintenances = append(cf.Sourdine.Maintenances, maintenanceExport{m.Cible, m.Debut, m.Fin, m.Recurrence, m.Commentaire})
		}
	}
	return cf, errors.Join(e[:]...)
}

// versYAML : passe par JSON pour garder les mêmes noms de champs partout.
func versYAML(v any) ([]byte, error) {
	j, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var gen any
	if err := json.Unmarshal(j, &gen); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(gen); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

// exportConfiguration : GET = sans les secrets ; POST = avec les secrets
// chiffrés, après le code 2FA ou en mode administration (audit du 16/11,
// SPEC § 34 : avant, un simple lien suffisait).
func (c *Console) exportConfiguration(q *req) {
	avec := false
	if q.r.Method == http.MethodPost {
		if err := c.verifierCode(q); err != nil {
			c.retour(q, "/reglages/export", err, "")
			return
		}
		avec = true
	} else if q.r.URL.Query().Get("secrets") != "" {
		c.retour(q, "/reglages/export", errors.New("export avec les secrets : utiliser le bouton de cette page (code 2FA demandé)"), "")
		return
	}
	cf, err := c.configuration(q.r.Context(), avec, q.t)
	if err != nil {
		http.Error(q.w, err.Error(), http.StatusInternalServerError)
		return
	}
	b, err := versYAML(cf)
	if err != nil {
		http.Error(q.w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = Auditer(q.r.Context(), c.db, q.qui(), "export_configuration", "", "", map[bool]string{true: "secrets chiffrés", false: "sans secret"}[avec], q.ip.String(), q.t)
	q.w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	q.w.Header().Set("Content-Disposition", `attachment; filename="mirador-configuration-`+q.t.Format("2006-01-02")+`.yaml"`)
	_, _ = q.w.Write(b)
}

// LireConfiguration : YAML → Configuration, champ inconnu = refus.
func LireConfiguration(r io.Reader) (Configuration, error) {
	var cf Configuration
	brut, err := io.ReadAll(io.LimitReader(r, 4<<20+1))
	if err != nil {
		return cf, err
	}
	if len(brut) > 4<<20 {
		return cf, errors.New("fichier trop gros (4 Mio au plus)")
	}
	var gen any
	if err := yaml.Unmarshal(brut, &gen); err != nil {
		return cf, fmt.Errorf("YAML illisible : %w", err)
	}
	j, err := json.Marshal(gen)
	if err != nil {
		return cf, fmt.Errorf("YAML non convertible : %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cf); err != nil {
		return cf, fmt.Errorf("configuration : %w", err)
	}
	if cf.Version != VersionExport {
		return cf, fmt.Errorf("version %d du format non gérée (attendue : %d)", cf.Version, VersionExport)
	}
	return cf, nil
}

// postImporterConfiguration : chaque partie est appliquée par sa fonction
// habituelle (mêmes contrôles, même audit). Rapport partie par partie.
func (c *Console) postImporterConfiguration(q *req) {
	ctx := q.r.Context()
	rapport, err := func() ([]string, error) {
		if err := q.r.ParseMultipartForm(4 << 20); err != nil {
			return nil, errors.New("fichier illisible (4 Mio au plus)")
		}
		defer q.r.MultipartForm.RemoveAll() //nolint:errcheck // en mémoire sous 4 Mio
		// Partie « securite » : nouveau code 2FA, toujours, comme la page
		// Sécurité (le mode administration ne suffit pas : audit du 16/11,
		// SPEC § 34). Les autres parties : code ou mode administration.
		verifier := c.verifierCode
		for _, p := range q.r.MultipartForm.Value["parties"] {
			if p == "securite" {
				verifier = c.verifierCodeFrais
			}
		}
		if err := verifier(q); err != nil {
			return nil, err
		}
		fh, _, err := q.r.FormFile("fichier")
		if err != nil {
			return nil, errors.New("aucun fichier")
		}
		defer fh.Close()
		cf, err := LireConfiguration(fh)
		if err != nil {
			return nil, err
		}
		parties := map[string]bool{}
		for _, p := range q.r.MultipartForm.Value["parties"] {
			parties[p] = true
		}
		var rap []string
		note := func(nom string, err error) {
			if err != nil {
				rap = append(rap, nom+" : ÉCHEC — "+err.Error())
			} else {
				rap = append(rap, nom+" : appliqué")
			}
		}
		qui := q.qui()
		if parties["regles"] {
			n, err := alertes.Enregistrer(ctx, c.db, cf.Regles, qui, q.t)
			note(fmt.Sprintf("règles (%d changée(s))", n), err)
		}
		if parties["notifications"] {
			cf.Notifications.Canaux.Tous = []string{} // un seul canal à la fois
			note("notifications", notif.EcrireReglages(ctx, c.db, cf.Notifications, c.opt.Cle, qui, q.t))
			note("comparaison vmalert", alertes.EcrireReglagesVmalert(ctx, c.db, cf.Vmalert, qui, q.t))
		}
		if parties["conservation"] {
			_, err := EcrireConservation(ctx, c.db, cf.Conservation, qui, q.ip.String(), q.t)
			note("conservation (une réduction attend 5 min)", err)
		}
		if parties["securite"] {
			if !cf.Securite.AdresseAutorisee(q.ip) {
				note("sécurité", errors.New("la liste d'adresses importée exclut votre adresse"))
			} else {
				_, err := EcrireSecurite(ctx, c.db, cf.Securite, qui, q.ip.String(), q.t)
				note("sécurité (liste d'adresses à confirmer)", err)
			}
		}
		if parties["sauvegarde"] {
			avant, err := sauvegarde.LireReglages(ctx, c.db)
			if err == nil {
				var r sauvegarde.Reglages
				if r, err = sauvegarde.PreparerSecrets(cf.Sauvegarde, avant, c.opt.Cle); err == nil {
					err = sauvegarde.EcrireReglages(ctx, c.db, r, qui, q.t)
				}
			}
			note("sauvegardes", err)
		}
		if parties["agents"] {
			ids := map[string]int64{}
			ms, _ := c.machines(ctx)
			for _, m := range ms {
				ids[m.Nom] = m.ID
			}
			for p, doc := range cf.ConfigAgents {
				portee := p
				if nom, ok := strings.CutPrefix(p, "machine:"); ok {
					if ids[nom] == 0 {
						note("configuration "+p, errors.New("machine inconnue ici"))
						continue
					}
					portee = "machine:" + strconv.FormatInt(ids[nom], 10)
				}
				// « *** » = secret actuel gardé (export sans les secrets).
				var actuel any
				var brut string
				if c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, "config_agent:"+portee).Scan(&brut) == nil {
					_ = json.Unmarshal([]byte(brut), &actuel)
				}
				garde, err := secrets.Garder(doc, actuel)
				if err != nil {
					note("configuration "+p, err)
					continue
				}
				b, _ := json.Marshal(garde)
				note("configuration "+p, flotte.DefinirConfig(ctx, c.db, portee, b, qui, q.t))
			}
			note("fenêtre des mises à jour", flotte.EcrireFenetre(ctx, c.db, cf.FenetreMaj, qui, q.t))
		}
		if parties["machines"] {
			for _, m := range cf.Machines {
				if _, err := flotte.IDMachine(ctx, c.db, m.Nom); err != nil {
					continue // seulement les machines déjà enrôlées ici
				}
				err := flotte.DefinirEtiquettes(ctx, c.db, m.Nom, m.Site, m.Env, m.Groupe, qui, q.t)
				if err == nil {
					err = flotte.DefinirIPs(ctx, c.db, m.Nom, m.IPsAutorisees, qui, q.t)
				}
				note("machine "+m.Nom, err)
			}
		}
		if parties["sourdine"] {
			for _, i := range cf.Sourdine.Inhibitions {
				_, err := notif.AjouterInhibition(ctx, c.db, i.Source, i.Cible, i.Egales, qui, i.Commentaire, q.t)
				note("inhibition "+i.Source, err)
			}
			for _, d := range cf.Sourdine.Dependances {
				_, err := notif.AjouterDependance(ctx, c.db, d.Enfant, d.Parent, qui, d.Commentaire, q.t)
				note("dépendance "+d.Enfant, err)
			}
			for _, m := range cf.Sourdine.Maintenances {
				_, err := notif.AjouterMaintenance(ctx, c.db, m.Cible, m.Debut, m.Fin, m.Recurrence, qui, m.Commentaire, q.t)
				note("maintenance "+m.Cible, err)
			}
		}
		if parties["interface"] {
			// Même chemin que le formulaire : ligne d'audit, et le mode
			// debug n'est jamais importé (il garde sa fin actuelle, bornée
			// à 240 min par le formulaire).
			actuel := lireInterface(ctx, c.db)
			cf.Interface.DebugJusqua = actuel.DebugJusqua
			err := cf.Interface.valider()
			if err == nil {
				err = c.enregistrerInterface(ctx, actuel, cf.Interface, qui, q.ip.String(), q.t)
			}
			note("interface", err)
		}
		if len(rap) == 0 {
			return nil, errors.New("aucune partie choisie")
		}
		return rap, nil
	}()
	if err != nil {
		c.retour(q, "/reglages/export", err, "")
		return
	}
	c.rendre(q, "import", "Import de la configuration", "reglages", rapport)
}
