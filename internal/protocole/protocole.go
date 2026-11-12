// SPDX-License-Identifier: GPL-3.0-or-later

// Package protocole décrit les échanges entre mirador-agent et mirador-server
// (SPEC.md § 5.2). Il est partagé par les deux binaires.
//
// Règles :
//   - l'agent ouvre toujours la connexion (HTTPS sortant) ; le serveur ne lui
//     envoie jamais rien de lui-même ;
//   - la version du protocole est dans l'URL (/api/agent/v1/...) ; le serveur
//     accepte au moins la version courante et la précédente ;
//   - les réponses ne contiennent que des valeurs typées : jamais de commande,
//     de chemin d'exécutable ni de script.
package protocole

import (
	"strconv"
	"time"
)

// Version du protocole agent → serveur.
const Version = 1

// Chemins des points d'entrée (préfixés par l'adresse du serveur).
const (
	CheminEnrolement = "/api/agent/v1/enroll"    // POST : demande d'enrôlement
	CheminMetriques  = "/api/agent/v1/metrics"   // POST : remote write (protobuf + snappy)
	CheminLogs       = "/api/agent/v1/logs"      // POST : jsonline (gzip)
	CheminConfig     = "/api/agent/v1/config"    // GET  : configuration (ETag)
	CheminMaj        = "/api/agent/v1/update"    // GET  : version assignée (ETag)
	CheminBinaire    = "/api/agent/v1/binary"    // GET  : binaire signé d'une version
	CheminBattement  = "/api/agent/v1/heartbeat" // POST : « je suis vivant » + état
	// Installation (jeton d'enrôlement dans « Authorization: Bearer ») :
	CheminInstallation        = "/api/agent/v1/install"        // GET : script d'installation
	CheminInstallationBinaire = "/api/agent/v1/install/binary" // GET : binaire (?arch=amd64|arm64)
	CheminCles                = "/api/agent/v1/cles"           // GET : clés publiques de signature (texte)
)

// En-têtes envoyés par l'agent.
const (
	EnteteProtocole = "X-Mirador-Protocole" // "1"
	EnteteVersion   = "X-Mirador-Version"   // version de l'agent
	// Heure de l'agent au moment de l'envoi (millisecondes Unix) : le serveur
	// mesure le décalage d'horloge, même pour un lot resté en file d'attente.
	EnteteHeure = "X-Mirador-Heure"
	// Capacités de l'agent, séparées par des virgules. Un ancien serveur
	// ignore cet en-tête ; un ancien agent ne l'envoie pas, et le serveur ne
	// lui envoie alors aucun réglage qu'il ne connaît pas.
	EnteteCapacites = "X-Mirador-Capacites"
)

// CapaciteSurveillance : l'agent connaît les réglages de surveillance choisis
// dans l'interface (SPEC.md § 19) et signale ce qu'il détecte sur la machine.
const CapaciteSurveillance = "surveillance"

// DemandeEnrolement : envoyée avec un jeton d'enrôlement à usage unique,
// généré dans l'interface (SPEC.md, « Enrôlement des machines »).
type DemandeEnrolement struct {
	Jeton        string `json:"jeton"`        // usage unique, durée limitée
	NomPropose   string `json:"nom_propose"`  // nom de la machine (modifiable dans l'interface)
	Version      string `json:"version"`      // version de l'agent
	Architecture string `json:"architecture"` // amd64, arm64
	Identifiant  string `json:"identifiant"`  // identifiant aléatoire de cette installation
	// Site et type proposés (installation : --site / --env, ou ceux d'Alloy).
	// Ceux du jeton ont la priorité ; l'admin les corrige à l'approbation.
	SitePropose string `json:"site_propose,omitempty"`
	EnvPropose  string `json:"env_propose,omitempty"`
}

// Statuts d'enrôlement.
const (
	EnAttente = "en_attente" // la machine attend l'approbation dans l'interface
	Approuve  = "approuve"   // le token définitif est fourni
	Refuse    = "refuse"     // jeton invalide, expiré ou demande rejetée
)

// ReponseEnrolement : le token définitif n'est donné qu'une fois, à
// l'approbation. Le serveur n'en garde que l'empreinte (hachage).
type ReponseEnrolement struct {
	Statut  string `json:"statut"`
	Token   string `json:"token,omitempty"`
	Nom     string `json:"nom,omitempty"`
	Attente int    `json:"attente_s,omitempty"` // délai conseillé avant de redemander
	Message string `json:"message,omitempty"`
}

// Battement : envoyé régulièrement ; sert au serveur à détecter un agent muet,
// une mise à jour réussie ou un retour arrière.
type Battement struct {
	Version      string    `json:"version"`
	Architecture string    `json:"architecture"` // amd64, arm64 (choix du binaire de mise à jour)
	Demarrage    time.Time `json:"demarrage"`
	MajEssai     string    `json:"maj_essai,omitempty"` // version en cours d'essai
	MajEchec     string    `json:"maj_echec,omitempty"` // dernier échec de mise à jour
}

// Bornes des messages JSON de l'agent (le serveur refuse au-delà).
const (
	TailleMaxDemande   = 4 << 10 // enrôlement, battement
	DelaiEnrolementDef = 30      // secondes entre deux demandes d'enrôlement
)

// ParametreVersion, ParametreArchitecture : paramètres de CheminBinaire.
const (
	ParametreVersion      = "version"
	ParametreArchitecture = "arch"
)

// ReponseMaj : version assignée à cette machine par l'interface.
type ReponseMaj struct {
	Manifeste *Manifeste `json:"manifeste,omitempty"` // nil : rester à la version actuelle
}

// Manifeste : description signée d'une version de mirador-agent, produite par
// mirador-release sur le poste de l'utilisateur (le serveur ne signe jamais).
type Manifeste struct {
	Version        string `json:"version"`         // 1.2.3
	Architecture   string `json:"architecture"`    // amd64, arm64
	SHA256         string `json:"sha256"`          // empreinte du binaire, hexadécimal
	Taille         int64  `json:"taille"`          // octets
	RetourAutorise bool   `json:"retour_autorise"` // instruction signée : accepter une version plus ancienne
	Signature      string `json:"signature"`       // Ed25519, base64, sur MessageSigne()
}

// MessageSigne renvoie exactement les octets signés. Tout changement d'un
// champ (version, empreinte, taille, retour arrière) casse la signature.
func (m *Manifeste) MessageSigne() []byte {
	retour := "non"
	if m.RetourAutorise {
		retour = "oui"
	}
	return []byte("mirador-agent\n" + m.Version + "\n" + m.Architecture + "\n" +
		m.SHA256 + "\n" + strconv.FormatInt(m.Taille, 10) + "\n" + retour + "\n")
}
