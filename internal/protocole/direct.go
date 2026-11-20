// SPDX-License-Identifier: GPL-3.0-or-later

package protocole

import (
	"errors"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mode « direct » de la page d'une machine (SPEC § 97, demande du 20/11) :
// quand quelqu'un regarde la page, l'agent envoie un petit relevé chaque
// seconde (CPU, mémoire, réseau, disques, processus les plus gourmands).
//
// L'agent n'ouvre toujours aucun port :
//   - le serveur répond à la demande de configuration gardée ouverte
//     (SPEC § 22) avec l'en-tête X-Mirador-Direct : secondes restantes ;
//   - l'agent envoie alors un relevé par seconde (POST CheminDirect) ; chaque
//     réponse redonne les secondes restantes (0 : arrêt).
// Rien n'est exécuté : l'agent lit /proc, comme pour ses métriques.

const (
	// EnteteDirect : secondes de direct restantes (réponse du serveur).
	EnteteDirect = "X-Mirador-Direct"
	// EnteteDirectActif : l'agent envoie déjà ses relevés (« 1 ») : le
	// serveur n'a pas besoin de le réveiller.
	EnteteDirectActif = "X-Mirador-Direct-Actif"
	// CapaciteDirect : l'agent sait faire le direct.
	CapaciteDirect = "direct"
	// DirectMaxProcessus : processus envoyés au plus par relevé.
	DirectMaxProcessus = 10
	// TailleMaxDirect : taille d'un relevé (octets).
	TailleMaxDirect = 8 << 10
)

// ProcessusDirect : un processus parmi les plus gourmands.
type ProcessusDirect struct {
	PID         int     `json:"pid"`
	Nom         string  `json:"nom"`
	Utilisateur string  `json:"util,omitempty"`
	CPU         float64 `json:"cpu"` // % d'un cœur (peut dépasser 100)
	Memoire     float64 `json:"mem"` // octets (mémoire résidente)
	// Groupe : VM, conteneur LXC ou Docker, d'après le cgroup (« VM 102 »,
	// « CT 200 », « docker 3f2a1b »).
	Groupe string `json:"groupe,omitempty"`
}

// Direct : un relevé (une seconde).
type Direct struct {
	T           int64             `json:"t"` // heure de l'agent (ms)
	CPU         float64           `json:"cpu"`
	Coeurs      int               `json:"coeurs"`
	Charge      float64           `json:"charge"`
	MemUtilisee float64           `json:"mem_utilisee"`
	MemTotale   float64           `json:"mem_totale"`
	NetRecu     float64           `json:"net_recu"` // octets/s
	NetEnvoye   float64           `json:"net_envoye"`
	DisqueLu    float64           `json:"disque_lu"` // octets/s
	DisqueEcrit float64           `json:"disque_ecrit"`
	Processus   []ProcessusDirect `json:"processus,omitempty"`
}

// Valider : bornes d'un relevé reçu (le serveur refuse au-delà) ; les noms
// sont gardés sur une ligne, sans caractère de contrôle, 64 caractères au plus.
func (d *Direct) Valider() error {
	nombres := []float64{d.CPU, d.Charge, d.MemUtilisee, d.MemTotale, d.NetRecu, d.NetEnvoye, d.DisqueLu, d.DisqueEcrit}
	for _, v := range nombres {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e15 {
			return errors.New("valeur hors bornes")
		}
	}
	if d.CPU > 100 || d.Coeurs < 0 || d.Coeurs > 4096 || len(d.Processus) > DirectMaxProcessus {
		return errors.New("relevé hors bornes")
	}
	for i := range d.Processus {
		p := &d.Processus[i]
		if p.PID <= 0 || math.IsNaN(p.CPU) || p.CPU < 0 || p.CPU > 409600 || math.IsNaN(p.Memoire) || p.Memoire < 0 || p.Memoire > 1e15 {
			return errors.New("processus hors bornes")
		}
		p.Nom, p.Utilisateur, p.Groupe = texteDirect(p.Nom), texteDirect(p.Utilisateur), texteDirect(p.Groupe)
	}
	return nil
}

func texteDirect(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	for utf8.RuneCountInString(s) > 64 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return strings.TrimSpace(s)
}
