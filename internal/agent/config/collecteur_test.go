// SPDX-License-Identifier: GPL-3.0-or-later

package config

import "testing"

func TestSyslogValider(t *testing.T) {
	bon := Syslog{Ecoute: "192.168.1.10:6514", NomCertificat: "syslog.lan.exemple.org",
		Emetteurs: []Emetteur{{Adresse: "192.168.1.20", Machine: "nas"}, {Adresse: "10.0.0.0/8", Machine: "nas"}, {Adresse: "fd00::5", Machine: "nas"}}}
	if err := bon.Valider([]string{"nas"}); err != nil {
		t.Fatal(err)
	}
	for nom, f := range map[string]func(s *Syslog){
		"écoute publique":           func(s *Syslog) { s.Ecoute = "203.0.113.5:6514" },
		"toutes les adresses":       func(s *Syslog) { s.Ecoute = "0.0.0.0:6514" },
		"boucle locale":             func(s *Syslog) { s.Ecoute = "127.0.0.1:6514" },
		"nom d'hôte":                func(s *Syslog) { s.Ecoute = "collecteur.lan:6514" },
		"port privilégié":           func(s *Syslog) { s.Ecoute = "192.168.1.10:514" },
		"nom de certificat":         func(s *Syslog) { s.NomCertificat = "Pas Un Nom" },
		"émetteur public":           func(s *Syslog) { s.Emetteurs = []Emetteur{{Adresse: "8.8.8.8", Machine: "nas"}} },
		"émetteur non rattaché":     func(s *Syslog) { s.Emetteurs = []Emetteur{{Adresse: "192.168.1.2", Machine: "autre"}} },
		"nom d'équipement invalide": func(s *Syslog) { s.Emetteurs = []Emetteur{{Adresse: "192.168.1.2", Machine: "a b"}} },
		"débit":                     func(s *Syslog) { s.Debit = -1 },
	} {
		s := bon
		s.Emetteurs = append([]Emetteur(nil), bon.Emetteurs...)
		f(&s)
		if err := s.Valider([]string{"nas"}); err == nil {
			t.Errorf("%s : accepté", nom)
		}
	}
	if p, err := LireEmetteur("192.168.1.20"); err != nil || p.Bits() != 32 {
		t.Errorf("LireEmetteur : %v %v", p, err)
	}
	c := Collecteur{Equipements: []string{"nas", "nas"}}
	if c.Valider() == nil {
		t.Error("équipement en double accepté")
	}
}
