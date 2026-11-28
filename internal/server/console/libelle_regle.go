// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"strings"

	"mirador-stack/internal/server/alertes"
)

// libelleRegle : libellé lisible d'une règle (SPEC § 73), calculé par le
// paquet alertes (aussi utilisé dans les messages, SPEC § 82).
func libelleRegle(nom string) string { return alertes.Libelle(nom) }

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
	"autorisation": "En-têtes Authorization et Cookie", "cartes": "Cartes bancaires", "emails": "Adresses e-mail",
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
