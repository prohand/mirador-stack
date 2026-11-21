// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"testing"
	"time"
)

// Heure des sauvegardes (retour du 21/11) : saisie dans le fuseau de
// l'interface, enregistrée en UTC ; prochaine exécution.
func TestHeureSauvegarde(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Skip("fuseaux absents")
	}
	hiver := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	if got := heureLocale("02:43", paris, hiver); got != "03:43" {
		t.Errorf("UTC → Paris : %s", got)
	}
	if got, err := heureUTC("03:45", paris, hiver); err != nil || got != "02:45" {
		t.Errorf("Paris → UTC : %s %v", got, err)
	}
	if _, err := heureUTC("25:00", paris, hiver); err == nil {
		t.Error("heure invalide acceptée")
	}
	if p := prochaineSauvegarde("02:43", hiver); !p.Equal(time.Date(2026, 11, 22, 2, 43, 0, 0, time.UTC)) {
		t.Errorf("prochaine : %v", p)
	}
	if p := prochaineSauvegarde("11:00", hiver); !p.Equal(time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)) {
		t.Errorf("prochaine le jour même : %v", p)
	}
}
