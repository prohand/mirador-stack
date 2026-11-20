// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"strings"
	"unicode"
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

// libelleRegle rend le libellé lisible d'une règle d'alerte.
func libelleRegle(nom string) string {
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
	return majusculeDebut(texte)
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

// etatAlerte : état d'une alerte en clair (SPEC § 73).
func etatAlerte(etat string) string {
	switch etat {
	case "active":
		return "En cours"
	case "en_attente":
		return "En attente"
	case "resolue":
		return "Terminée"
	}
	return majusculeDebut(etat)
}

// nomRegleDe : exemple de nom technique tiré d'un titre (« Service systemd
// en échec » → « ServiceSystemdEnEchec »), pour l'aide du champ Nom de
// « Nouvelle règle » (audit du 19/11 : « ex. serviceEleve » était faux).
func nomRegleDe(titre string) string {
	sansAccent := strings.NewReplacer("é", "e", "è", "e", "ê", "e", "à", "a", "â", "a", "ô", "o", "û", "u", "ù", "u", "î", "i", "ï", "i", "ç", "c",
		"É", "E", "È", "E", "À", "A")
	var b strings.Builder
	for _, m := range strings.FieldsFunc(sansAccent.Replace(titre), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}) {
		b.WriteString(strings.ToUpper(m[:1]) + strings.ToLower(m[1:]))
	}
	return b.String()
}

// partieExport : nom lisible d'une partie de la configuration (Réglages ›
// Export) ; la valeur envoyée reste le nom technique.
func partieExport(p string) string {
	if l, ok := map[string]string{"regles": "Règles", "securite": "Sécurité", "sauvegarde": "Sauvegarde", "sourdine": "Silences et dépendances"}[p]; ok {
		return l
	}
	return majusculeDebut(p)
}

// groupeRegle : titre d'un groupe de règles (« hote » → « Hôte »).
func groupeRegle(g string) string {
	if l, ok := map[string]string{"hote": "Hôte", "materiel": "Matériel", "taches": "Tâches planifiées", "modeles": "Modèles", "services": "Applications", "mirador": "mirador"}[g]; ok {
		return l
	}
	return majusculeDebut(g)
}

// dureeFr : durée au format Prometheus (« 1h30m », « 3d », « 1w ») en
// français (« 1 h 30 min », « 3 j », « 1 sem. »). Texte inconnu : inchangé.
func dureeFr(v any) string {
	s := fmt.Sprint(v)
	unites := map[string]string{"ms": "ms", "s": "s", "m": "min", "h": "h", "d": "j", "w": "sem.", "y": "an"}
	var mots []string
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		k := j
		for k < len(s) && (s[k] < '0' || s[k] > '9') {
			k++
		}
		u, ok := unites[s[j:k]]
		if j == i || !ok {
			return s
		}
		if u == "an" && s[i:j] != "1" {
			u = "ans"
		}
		mots = append(mots, s[i:j]+" "+u)
		i = k
	}
	if len(mots) == 0 {
		return s
	}
	return strings.Join(mots, " ")
}

// libellesMasquages : noms des modèles de masquage des logs (agent) → en
// clair. Ce sont des libellés d'affichage, aucun secret.
var libellesMasquages = map[string]string{ // #nosec G101 -- libellés d'affichage, pas des secrets
	"mots_de_passe": "Mots de passe", "tokens": "Jetons", "cles_api": "Clés API",
	"autorisation": "En-têtes Authorization", "cartes": "Cartes bancaires", "emails": "Adresses e-mail",
}

// motTechnique : mots techniques courants montrés tels quels dans des
// listes (type d'envoi, canal, action de l'historique) → en clair.
// Inconnu : première lettre en majuscule.
func motTechnique(m string) string {
	if l, ok := libellesMasquages[m]; ok {
		return l
	}
	if l, ok := map[string]string{
		"debut": "Début", "fin": "Fin", "securite": "Sécurité", "test": "Test", // types d'envoi
		"ombre": "Mode ombre (rien envoyé)", "file": "File d'attente", "telegram": "Telegram", "olvid": "Olvid", "email": "E-mail", // canaux
		"regle": "Règle", "regle_supprimee": "Règle supprimée", "regle_desactivee": "Règle désactivée", "regle_activee": "Règle activée", // historique
		"planifiee": "Planifiée", "manuelle": "Manuelle", "ok": "OK", "Ok": "OK", "erreur": "Erreur", "echec": "Échec", // sauvegardes
	}[m]; ok {
		return l
	}
	return majusculeDebut(m)
}
