// SPDX-License-Identifier: GPL-3.0-or-later

package console

import "testing"

// Libellés lisibles des règles (SPEC § 73).
func TestLibelleRegle(t *testing.T) {
	cas := map[string]string{
		// écrits à la main
		"MiseAJourAgentEchouee": "Mise à jour de l'agent échouée",
		"AgentMuet":             "Agent muet",
		// découpe + accents
		"SondeEnEchec":          "Sonde en échec",
		"HorlogeDecalee":        "Horloge décalée",
		"ServiceSystemdEnEchec": "Service systemd en échec",
		"PoolZfsDegrade":        "Pool ZFS dégradé",
		"PareFeuNonRestrictif":  "Pare-feu non restrictif",
		// marque en tête
		"OpnsenseCpuEleve":      "OPNsense : CPU élevé",
		"UnifiMuet":             "UniFi muet",
		"SynologyVentilateurHS": "Synology : ventilateur HS",
		// sigle inconnu gardé, sigle suivi d'un mot coupé
		"NASInjoignable": "NAS injoignable",
		// règles écrites à la main par l'utilisateur
		"disque_plein_nas": "Disque plein nas",
		"x":                "X",
		"":                 "",
	}
	for nom, attendu := range cas {
		if got := libelleRegle(nom); got != attendu {
			t.Errorf("libelleRegle(%q) = %q, attendu %q", nom, got, attendu)
		}
	}
}

func TestEtatAlerte(t *testing.T) {
	for e, attendu := range map[string]string{"active": "En cours", "en_attente": "En attente", "resolue": "Terminée", "autre": "Autre"} {
		if got := etatAlerte(e); got != attendu {
			t.Errorf("etatAlerte(%q) = %q, attendu %q", e, got, attendu)
		}
	}
}
