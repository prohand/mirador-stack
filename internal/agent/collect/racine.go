// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"errors"
	"os"
	"time"

	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/racine"
)

// Racine lit le fichier écrit par l'assistant root (toutes les 15 min) et en
// tire les métriques, avec les noms actuels (SPEC.md § 4.2) :
//
//	node_firewall_input_policy_drop{tool,policy}, node_firewall_input_rules{tool,policy}
//	smartctl_device_smart_status{device}, smartctl_device_temperature{device,temperature_type}
//	smartctl_device_power_on_seconds{device}, smartctl_device_available_spare{device}
//	smartctl_device_attribute{device,attribute_name,attribute_value_type="raw"}
//	ssl_cert_not_after_timestamp_seconds{path,cn,issuer} (+ not_before)
//	ssl_certs_found_total, ssl_certs_read_errors_total
//
// Le fichier n'est relu que s'il a changé (date de modification). Les valeurs
// sont renvoyées à chaque passage (60 s) pour que les séries ne disparaissent
// pas. Un fichier trop vieux (assistant arrêté) n'est plus envoyé : seul son
// âge l'est (mirador_agent_root_age_seconds), pour pouvoir alerter.
type Racine struct {
	chemin     string
	ageMax     time.Duration
	maintenant func() time.Time

	modif    time.Time
	resultat *racine.Resultat

	// Résultats non envoyés (choix de l'interface, SPEC.md § 19). L'assistant
	// root tourne quand même : il ne lit jamais la configuration distante.
	SansPareFeu, SansSmart, SansCertificats bool
	// Certificats retirés un par un dans l'interface (chemins exacts) : lus
	// par l'assistant root, mais pas envoyés (SPEC.md § 21).
	certsIgnores map[string]bool
}

// IgnorerCertificats : chemins des certificats à ne pas envoyer.
func (r *Racine) IgnorerCertificats(chemins []string) {
	if len(chemins) == 0 {
		r.certsIgnores = nil
		return
	}
	r.certsIgnores = make(map[string]bool, len(chemins))
	for _, c := range chemins {
		r.certsIgnores[c] = true
	}
}

// NewRacine crée le collecteur.
func NewRacine(chemin string) *Racine {
	return &Racine{chemin: chemin, ageMax: 2 * time.Hour, maintenant: time.Now}
}

func (r *Racine) Nom() string { return "racine" }
func (r *Racine) Lent() bool  { return true }

func (r *Racine) Collecter(b *metrics.Batch) error {
	info, err := os.Stat(r.chemin)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // assistant pas (encore) installé ou pas encore passé
		}
		return err
	}
	if !info.ModTime().Equal(r.modif) {
		res, err := racine.Lire(r.chemin)
		if err != nil {
			return err
		}
		r.resultat, r.modif = res, info.ModTime()
	}
	res := r.resultat
	age := r.maintenant().Sub(res.Horodatage)
	b.Add("mirador_agent_root_age_seconds", age.Seconds())
	b.Add("mirador_agent_root_errors", float64(len(res.Erreurs)))
	// Version de l'assistant root : il n'est mis à jour qu'en relançant le
	// script d'installation (jamais par la mise à jour automatique) ;
	// l'interface signale un écart avec la version de l'agent.
	if res.Version != "" {
		b.Add("mirador_agent_root_info", 1, metrics.L("version", res.Version))
	}
	if age > r.ageMax {
		return nil
	}

	if pf := res.PareFeu; pf != nil && !r.SansPareFeu {
		l := [2]metrics.Label{metrics.L("tool", pf.Outil), metrics.L("policy", pf.Politique)}
		b.Add("node_firewall_input_policy_drop", bool01(pf.Restrictif()), l[:]...)
		b.Add("node_firewall_input_rules", float64(pf.Regles), l[:]...)
	}

	for i := range res.Smart {
		if r.SansSmart {
			break
		}
		d := &res.Smart[i]
		dev := metrics.L("device", d.Nom)
		if d.SanteOK != nil {
			b.Add("smartctl_device_smart_status", bool01(*d.SanteOK), dev)
		}
		if d.Temperature != nil {
			b.Add("smartctl_device_temperature", *d.Temperature, dev, metrics.L("temperature_type", "current"))
		}
		if d.HeuresMarche != nil {
			b.Add("smartctl_device_power_on_seconds", *d.HeuresMarche*3600, dev)
		}
		if d.ReserveNVMe != nil {
			b.Add("smartctl_device_available_spare", *d.ReserveNVMe, dev)
		}
		for nom, v := range d.Attributs {
			b.Add("smartctl_device_attribute", float64(v), dev,
				metrics.L("attribute_name", nom), metrics.L("attribute_value_type", "raw"))
		}
	}

	if c := res.Certificats; c != nil && !r.SansCertificats {
		envoyes := 0
		for i := range c.Liste {
			cert := &c.Liste[i]
			if r.certsIgnores[cert.Chemin] {
				continue
			}
			envoyes++
			l := [3]metrics.Label{metrics.L("path", cert.Chemin), metrics.L("cn", cert.CN), metrics.L("issuer", cert.Emetteur)}
			b.Add("ssl_cert_not_after_timestamp_seconds", float64(cert.Fin.Unix()), l[:]...)
			b.Add("ssl_cert_not_before_timestamp_seconds", float64(cert.Debut.Unix()), l[:]...)
		}
		// Compte rendu des chemins ajoutés dans l'interface.
		for _, m := range c.Motifs {
			ch := metrics.L("chemin", m.Motif)
			b.Add("mirador_agent_certificats_chemin_fichiers", float64(m.Fichiers), ch)
			b.Add("mirador_agent_certificats_chemin_lus", float64(m.Lus), ch)
		}
		// Trouvés = certificats envoyés (les retirés ne comptent plus).
		b.Add("ssl_certs_found_total", float64(envoyes))
		b.Add("ssl_certs_read_errors_total", float64(c.Erreurs))
	}
	return nil
}
