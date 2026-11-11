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
	if age > r.ageMax {
		return nil
	}

	if pf := res.PareFeu; pf != nil {
		l := [2]metrics.Label{metrics.L("tool", pf.Outil), metrics.L("policy", pf.Politique)}
		b.Add("node_firewall_input_policy_drop", bool01(pf.Restrictif()), l[:]...)
		b.Add("node_firewall_input_rules", float64(pf.Regles), l[:]...)
	}

	for i := range res.Smart {
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

	if c := res.Certificats; c != nil {
		for i := range c.Liste {
			cert := &c.Liste[i]
			l := [3]metrics.Label{metrics.L("path", cert.Chemin), metrics.L("cn", cert.CN), metrics.L("issuer", cert.Emetteur)}
			b.Add("ssl_cert_not_after_timestamp_seconds", float64(cert.Fin.Unix()), l[:]...)
			b.Add("ssl_cert_not_before_timestamp_seconds", float64(cert.Debut.Unix()), l[:]...)
		}
		b.Add("ssl_certs_found_total", float64(c.Trouves))
		b.Add("ssl_certs_read_errors_total", float64(c.Erreurs))
	}
	return nil
}
