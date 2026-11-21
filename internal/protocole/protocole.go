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
	// CheminInflux : entrée InfluxDB v2 (Telegraf : …/api/v2/write, …/health).
	CheminInflux = "/api/agent/v1/influx"
	// Installation (jeton d'enrôlement dans « Authorization: Bearer ») :
	CheminInstallation        = "/api/agent/v1/install"        // GET : script d'installation
	CheminInstallationBinaire = "/api/agent/v1/install/binary" // GET : binaire (?arch=amd64|arm64[&produit=mirador-collector])
	// CheminInstallationCollecteur : script d'installation de mirador-collector.
	CheminInstallationCollecteur = "/api/agent/v1/install/collecteur"
	CheminCles                   = "/api/agent/v1/cles" // GET : clés publiques de signature (texte)
	// CheminDirect : relevé « en direct » de la page d'une machine (SPEC § 97).
	CheminDirect = "/api/agent/v1/direct" // POST : JSON (Direct)
	// CheminEvenement : panne vue par l'agent tout de suite (SPEC § 98).
	CheminEvenement = "/api/agent/v1/evenement" // POST : JSON (EvenementAgent)
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
	// Configuration en temps réel (SPEC § 22) : l'agent demande au serveur
	// de garder la requête ouverte (secondes, 45 au plus) et dit quelle
	// demande « Actualiser » il a déjà traitée ; le serveur dit qu'il sait
	// attendre et donne le numéro de la dernière demande « Actualiser ».
	EnteteAttendre   = "X-Mirador-Attendre"
	EnteteActualise  = "X-Mirador-Actualise"
	EnteteAttente    = "X-Mirador-Attente"
	EnteteActualiser = "X-Mirador-Actualiser"
	// Empreinte de la machine (32 chiffres hexadécimaux, dérivée de
	// /etc/machine-id). Si l'admin a lié le token à la machine, le serveur
	// refuse ce token avec une autre empreinte (token copié ailleurs).
	EnteteEmpreinte = "X-Mirador-Empreinte"
)

// CapaciteSurveillance : l'agent connaît les réglages de surveillance choisis
// dans l'interface (SPEC.md § 19) et signale ce qu'il détecte sur la machine.
const CapaciteSurveillance = "surveillance"

// CapaciteElements : l'agent sait retirer un disque, une interface, un
// montage ou un certificat précis, et chercher des certificats aux chemins
// ajoutés dans l'interface (SPEC.md § 21).
const CapaciteElements = "elements"

// CapaciteProxmox : l'agent connaît le réglage « proxmox_disques » (SPEC.md § 23.7).
const CapaciteProxmox = "proxmox"

// CapaciteChecks : l'agent sait faire les checks des modèles (SPEC.md § 24.10).
const CapaciteChecks = "checks"

// CapaciteCollecteur : mirador-collector (clé « collecteur » de la
// configuration, SPEC § 23.13).
const CapaciteCollecteur = "collecteur"

// CapaciteReessai : l'agent comprend « reessai » dans la réponse de mise à
// jour (bouton « Réessayer la mise à jour », SPEC § 44) : il retente une
// version déjà en échec si la demande est plus récente que l'échec. Le
// serveur n'envoie ce champ qu'aux agents qui l'annoncent (les anciens
// refusent tout champ inconnu).
const CapaciteReessai = "reessai"

// CapaciteSondes : mirador-collector fait les sondes de services (clé
// « services » de « collecteur », SPEC § 48). Un collecteur plus ancien
// refuserait la clé : le serveur la lui retire.
const CapaciteSondes = "sondes"

// CapaciteSonder : mirador-collector sait faire tout de suite l'essai d'une
// sonde demandé dans l'interface (bouton « Vérifier maintenant », SPEC
// § 108), via EnteteSonde / EnteteSonder. Un collecteur plus ancien n'envoie
// pas EnteteSonde : le bouton est alors grisé pour ses sondes.
const CapaciteSonder = "sonder"

// EnteteSonde (requête) : numéro de la dernière demande d'essai déjà faite
// par le collecteur (0 au démarrage). EnteteSonder (réponse) : « N:nom1,nom2 »,
// N = numéro de la demande la plus récente, noms des sondes à essayer
// (échappés comme dans une URL), seulement celles plus récentes que
// EnteteSonde. Rien d'autre ne peut être demandé : le collecteur n'essaie
// que des sondes déjà dans sa configuration.
const (
	EnteteSonde  = "X-Mirador-Sonde"
	EnteteSonder = "X-Mirador-Sonder"
)

// Capacites : ce que cet agent annonce (en-tête X-Mirador-Capacites).
const Capacites = CapaciteSurveillance + "," + CapaciteElements + "," + CapaciteProxmox + "," + CapaciteChecks + "," + CapaciteReessai + "," + CapaciteDirect

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
	// CSRSyslog : demande de certificat (PEM) du syslog de mirador-collector
	// (SPEC § 23.14) ; le serveur fait la démarche ACME et renvoie le
	// certificat dans la configuration.
	CSRSyslog string `json:"csr_syslog,omitempty"`
}

// Bornes des messages JSON de l'agent (le serveur refuse au-delà).
const (
	TailleMaxDemande   = 8 << 10 // enrôlement, battement (CSR du syslog comprise)
	DelaiEnrolementDef = 30      // secondes entre deux demandes d'enrôlement
)

// ParametreVersion, ParametreArchitecture : paramètres de CheminBinaire.
const (
	ParametreVersion      = "version"
	ParametreArchitecture = "arch"
	ParametreProduit      = "produit" // installation : mirador-agent (défaut) ou mirador-collector
)

// ReponseMaj : version assignée à cette machine par l'interface.
type ReponseMaj struct {
	Manifeste *Manifeste `json:"manifeste,omitempty"` // nil : rester à la version actuelle
	// Reessai : heure (secondes Unix) du dernier « Réessayer la mise à jour »
	// demandé dans l'interface. Seulement pour un agent qui annonce
	// CapaciteReessai.
	Reessai int64 `json:"reessai,omitempty"`
}

// Produits signés (SPEC § 23.13) : le nom du produit fait partie du message
// signé, un binaire de l'agent ne peut donc pas être installé comme
// collecteur, ni l'inverse.
const (
	ProduitAgent      = "mirador-agent"
	ProduitCollecteur = "mirador-collector"
	// ProduitImageServeur : image Docker du serveur (SPEC § 102). Le
	// manifeste ne décrit pas un binaire : SHA256 est l'empreinte de l'image
	// (« sha256:… » sans le préfixe), Taille vaut 0, Architecture « amd64 ».
	// Lu seulement par mirador-maj-serveur (VPS) ; un agent ne peut pas
	// l'installer (produit différent, et aucun binaire n'a cette empreinte).
	ProduitImageServeur = "mirador-server-image"
)

// Manifeste : description signée d'une version de mirador-agent (ou de
// mirador-collector), produite par mirador-release sur le poste de
// l'utilisateur (le serveur ne signe jamais).
type Manifeste struct {
	// Produit : vide pour mirador-agent (manifestes d'avant l'étape 6, que les
	// agents déjà installés savent lire), « mirador-collector » sinon.
	Produit        string `json:"produit,omitempty"`
	Version        string `json:"version"`         // 1.2.3
	Architecture   string `json:"architecture"`    // amd64, arm64
	SHA256         string `json:"sha256"`          // empreinte du binaire, hexadécimal
	Taille         int64  `json:"taille"`          // octets
	RetourAutorise bool   `json:"retour_autorise"` // instruction signée : accepter une version plus ancienne
	// RetourJusqua : fin (secondes Unix) de l'autorisation de retour arrière,
	// signée avec elle (relecture du 21/11 : sans limite, un manifeste
	// « retour autorisé » signé une fois permettait, des mois plus tard, à un
	// serveur piraté de ramener le parc vers une version à faille connue).
	// 0 : pas de date (manifeste ancien) : retour refusé par les agents.
	RetourJusqua int64  `json:"retour_jusqua,omitempty"`
	Signature    string `json:"signature"` // Ed25519, base64, sur MessageSigne()
}

// ProduitEffectif : produit du manifeste (mirador-agent si vide).
func (m *Manifeste) ProduitEffectif() string {
	if m.Produit == "" {
		return ProduitAgent
	}
	return m.Produit
}

// MessageSigne renvoie exactement les octets signés. Tout changement d'un
// champ (version, empreinte, taille, retour arrière) casse la signature.
func (m *Manifeste) MessageSigne() []byte {
	retour := "non"
	if m.RetourAutorise {
		retour = "oui"
	}
	msg := m.ProduitEffectif() + "\n" + m.Version + "\n" + m.Architecture + "\n" +
		m.SHA256 + "\n" + strconv.FormatInt(m.Taille, 10) + "\n" + retour + "\n"
	// Date de fin du retour arrière : ajoutée seulement si elle existe (les
	// manifestes sans retour arrière gardent exactement le même message).
	if m.RetourJusqua != 0 {
		msg += strconv.FormatInt(m.RetourJusqua, 10) + "\n"
	}
	return []byte(msg)
}
