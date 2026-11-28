// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Libellé lisible d'une règle d'alerte (SPEC § 73) : le nom technique
// « MiseAJourAgentEchouee » devient « Mise à jour de l'agent échouée ».
// Affichage seulement : le nom technique reste l'identifiant (filtres,
// liens, export YAML, notifications) et s'affiche en infobulle.
//
// 1. Libellés écrits à la main pour les règles connues (défauts mirador
//    et règles courantes de l'import vmalert).
// 2. Sinon : le nom est coupé aux majuscules, mis en minuscules, et les
//    mots connus reprennent leurs accents (« echouee » → « échouée ») ou
//    leur écriture (« cpu » → « CPU », « opnsense » → « OPNsense »).

// libellesRegles : règles dont la découpe automatique ne donne pas une
// phrase naturelle.
var libellesRegles = map[string]string{
	"AgentMuet":                            "Agent muet",
	"MiseAJourAgentEchouee":                "Mise à jour de l'agent échouée",
	"MisesAJourSecuriteEnAttente":          "Mises à jour de sécurité en attente",
	"OomKiller":                            "Processus tué (manque de mémoire)",
	"ConteneurTueParOOM":                   "Conteneur tué (manque de mémoire)",
	"ServiceDuModeleAbsent":                "Service du modèle absent",
	"ServeurDisqueSeRemplitVite":           "Disque du serveur : se remplit vite",
	"ServeurDisquePresquePlein":            "Disque du serveur presque plein",
	"EvaluationAlertesLente":               "Évaluation des alertes lente",
	"ReglesEnErreur":                       "Règles d'alerte en erreur",
	"SauvegardeTropAncienne":               "Sauvegarde trop ancienne",
	"ProxmoxInviteCpuSature":               "Proxmox : CPU d'un invité saturé",
	"ProxmoxConteneurMemoireSaturee":       "Proxmox : mémoire d'un conteneur saturée",
	"ProxmoxListeInvitesIndisponible":      "Proxmox : liste des invités indisponible",
	"CertificatSyslogBientotExpire":        "Certificat syslog bientôt expiré",
	"SondeCertificatBientotExpire":         "Certificat d'un site bientôt expiré",
	"CertificatSyslogRenouvellementEchoue": "Renouvellement du certificat syslog échoué",
	"IoDisqueSature":                       "Disque saturé (entrées / sorties)",
	"LoadAverageEleve":                     "Charge système élevée",
	"SynologyMiseAJourDSM":                 "Synology : mise à jour DSM disponible",
	"UnifiMiseAJourDisponible":             "UniFi : mise à jour disponible",
	"TacheEnEchec":                         "Tâche planifiée en échec",
	"TacheEnEchecCritique":                 "Tâche planifiée en échec",
	"TacheEnRetard":                        "Tâche planifiée en retard",
	"TacheEnRetardCritique":                "Tâche planifiée en retard",
	"TacheAnormale":                        "Tâche planifiée inhabituelle",
	"VMSansSauvegarde":                     "VM sans sauvegarde planifiée",
	"Watchdog":                             "Watchdog (test de la chaîne d'alerte)",
	"OpnsenseRedemarre":                    "OPNsense a redémarré",
	"UnifiEquipementRedemarre":             "UniFi : équipement redémarré",
	"ConteneurMemoireSaturee":              "Mémoire du conteneur saturée",
	"CollecteCertificatsMuette":            "Collecte des certificats muette",
	"DisqueSaturationPrevue":               "Disque bientôt plein (prévision)",
}

// motsRegles : écriture des mots une fois le nom découpé et mis en
// minuscules (accents, sigles, marques).
var motsRegles = map[string]string{
	// accents
	"a": "à", "arrete": "arrêté", "arretee": "arrêtée",
	"bientot": "bientôt", "decalee": "décalée", "decale": "décalé",
	"deconnecte": "déconnecté", "defectueux": "défectueux",
	"degrade": "dégradé", "degradee": "dégradée", "echec": "échec",
	"echoue": "échoué", "echouee": "échouée", "eleve": "élevé", "elevee": "élevée",
	"equipement": "équipement", "equipements": "équipements",
	"etat": "état", "etats": "états", "evaluation": "évaluation",
	"memoire": "mémoire",
	"modele":  "modèle", "prevue": "prévue", "realloues": "réalloués",
	"redemarrage": "redémarrage", "redemarre": "redémarre",
	"reparation": "réparation", "reseau": "réseau", "saturee": "saturée",
	"sature": "saturé", "securite": "sécurité", "systeme": "système",
	"temperature": "température", "tres": "très", "tue": "tué",
	"utilise": "utilisé", "detecte": "détecté", "degradation": "dégradation",
	"donnees": "données", "delai": "délai",
	// sigles et marques
	"cpu": "CPU", "dns": "DNS", "dsm": "DSM", "hs": "HS", "http": "HTTP",
	"https": "HTTPS", "oom": "OOM", "raid": "RAID", "smart": "SMART",
	"ssl": "SSL", "tcp": "TCP", "tls": "TLS", "ups": "UPS", "vpn": "VPN",
	"zfs": "ZFS", "io": "E/S",
	"docker": "Docker", "opnsense": "OPNsense", "proxmox": "Proxmox",
	"synology": "Synology", "unifi": "UniFi", "systemd": "systemd",
}

// expressionsRegles : suites de mots à réécrire après la découpe.
var expressionsRegles = []struct{ de, vers string }{
	{"pare feu", "pare-feu"},
	{"load average", "charge système"},
	{"pertes paquets", "pertes de paquets"},
	{"erreurs interface", "erreurs d'interface"},
	{"erreurs port", "erreurs de port"},
	{"table états", "table d'états"},
}

// marquesRegles : un nom qui commence par une marque devient
// « Marque : reste » (« OPNsense : CPU élevé »), sauf si le reste tient en
// un mot (« OPNsense muet »).
var marquesRegles = map[string]bool{
	"Docker": true, "OPNsense": true, "Proxmox": true, "Synology": true, "UniFi": true,
}

// Libelle rend le libellé lisible d'une règle d'alerte.
func Libelle(nom string) string {
	if l, ok := libellesRegles[nom]; ok {
		return l
	}
	mots := couperNom(nom)
	if len(mots) == 0 {
		return nom
	}
	for i, m := range mots {
		bas := strings.ToLower(m)
		if v, ok := motsRegles[bas]; ok {
			mots[i] = v
		} else if estSigle(m) {
			mots[i] = m // sigle inconnu (« NAS ») : gardé tel quel
		} else {
			mots[i] = bas
		}
	}
	texte := strings.Join(mots, " ")
	if len(mots) > 2 && marquesRegles[mots[0]] {
		texte = mots[0] + " : " + strings.Join(mots[1:], " ")
	}
	for _, e := range expressionsRegles {
		texte = strings.ReplaceAll(texte, e.de, e.vers)
	}
	return majuscule(texte)
}

// couperNom découpe « ServiceSystemdEnEchec » en [Service Systemd En
// Echec], « ConteneurTueParOOM » en [Conteneur Tue Par OOM],
// « disque_plein » en [disque plein]. Un sigle suivi d'un mot
// (« HTTPCode ») est coupé avant la dernière majuscule.
func couperNom(nom string) []string {
	var mots []string
	r := []rune(nom)
	debut := 0
	ajouter := func(fin int) {
		if fin > debut {
			mots = append(mots, string(r[debut:fin]))
		}
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		if c == '_' || c == '-' || c == ' ' {
			ajouter(i)
			debut = i + 1
			continue
		}
		if i == debut {
			continue
		}
		p := r[i-1]
		switch {
		// minuscule ou chiffre → majuscule : nouveau mot
		case unicode.IsUpper(c) && (unicode.IsLower(p) || unicode.IsDigit(p)):
			ajouter(i)
			debut = i
		// fin d'un sigle : « OOMKiller » → OOM | Killer
		case unicode.IsUpper(c) && unicode.IsUpper(p) && i+1 < len(r) && unicode.IsLower(r[i+1]):
			ajouter(i)
			debut = i
		}
	}
	ajouter(len(r))
	return mots
}

// estSigle : mot d'au moins deux lettres, toutes en majuscules.
func estSigle(m string) bool {
	n := 0
	for _, c := range m {
		if unicode.IsLetter(c) {
			if !unicode.IsUpper(c) {
				return false
			}
			n++
		}
	}
	return n >= 2
}

// majuscule : première lettre en majuscule.
func majuscule(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
