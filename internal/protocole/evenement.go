// SPDX-License-Identifier: GPL-3.0-or-later

package protocole

import (
	"errors"
	"net/netip"
	"regexp"
)

// Pannes vues tout de suite (SPEC § 98) : l'agent écoute systemd (D-Bus) et
// le compteur du noyau « oom_kill » ; dès qu'un service tombe ou qu'un
// processus est tué faute de mémoire, il envoie ses métriques sans attendre
// le relevé suivant et prévient le serveur, qui évalue les alertes aussitôt
// et note la panne dans le journal des événements.

// Types d'événement.
const (
	EvenementService       = "service"        // unité systemd passée en échec
	EvenementServiceRetour = "service-retour" // unité sortie de l'échec (SPEC § 127)
	EvenementOOM           = "oom"            // processus tué faute de mémoire
	// EvenementSSH : connexion SSH réussie (demande du 27/11). Objet :
	// adresse source ; Texte : « utilisateur · méthode ». Le serveur ne
	// garde que les adresses publiques jamais vues sur la machine.
	EvenementSSH = "ssh"
)

// EvenementAgent : une panne vue par l'agent.
type EvenementAgent struct {
	Type  string `json:"type"`
	Objet string `json:"objet,omitempty"` // nom de l'unité (service)
	Texte string `json:"texte,omitempty"` // « exit-code », « 1 processus »…
	Le    int64  `json:"le"`              // heure de l'agent (ms)
}

var reUnite = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]{1,128}$`)

// Valider : type connu, textes bornés.
func (e *EvenementAgent) Valider() error {
	switch e.Type {
	case EvenementService, EvenementServiceRetour:
		if !reUnite.MatchString(e.Objet) {
			return errors.New("unité invalide")
		}
	case EvenementOOM:
		e.Objet = ""
	case EvenementSSH:
		a, err := netip.ParseAddr(e.Objet)
		if err != nil {
			return errors.New("adresse invalide")
		}
		e.Objet = a.Unmap().String()
	default:
		return errors.New("type inconnu")
	}
	e.Texte = texteDirect(e.Texte)
	return nil
}
