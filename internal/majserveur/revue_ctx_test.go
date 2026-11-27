// SPDX-License-Identifier: GPL-3.0-or-later

package majserveur

import (
	"context"
	"testing"
	"time"
)

// dockerCtx : comme le vrai (exec.CommandContext) : contexte fini = échec.
type dockerCtx struct{ *dockerSimule }

func (d dockerCtx) Lancer(ctx context.Context, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		d.appels = append(d.appels, "ÉCHEC(ctx) "+args[0]+" "+args[1])
		return "", err
	}
	return d.dockerSimule.Lancer(ctx, args...)
}

// DELAI=30m (accepté par LireReglages) et nouvelle version qui ne démarre
// pas : le contexte global de 25 min (cmd/mirador-maj-serveur) expire pendant
// l'attente ; le retour arrière n'est alors jamais fait.
func TestRevueRetourArriereContexteExpire(t *testing.T) {
	b := nouveauBanc(t)
	b.h.R.Delai = 30 * time.Minute
	b.d.demarre["0.6.0"] = false // la nouvelle version ne démarre pas
	b.h.Docker = dockerCtx{b.d}
	ctx, fin := context.WithCancel(context.Background())
	defer fin()
	debut := b.horloge
	b.h.Dormir = func(d time.Duration) {
		b.horloge = b.horloge.Add(d)
		if b.horloge.Sub(debut) >= 25*time.Minute {
			fin() // = context.WithTimeout(…, 25*time.Minute) de main.go
		}
	}
	b.publier(t, "0.6.0")
	b.demander(t, "0.6.0")
	err := b.h.Appliquer(ctx)
	_ = err // échec attendu (nouvelle version refusée) : l'état décrit le retour
	// Base remise puis ancienne version relancée (qui l'ouvre : « +0.5.0 »).
	if got := string(b.d.fichiers[b.h.R.FichierBase]); got != "base-0.5.0+0.5.0" {
		t.Errorf("base non remise : %q", got)
	}
	e := b.etat(t)
	if e.Message != "la version 0.6.0 n'a pas démarré : retour à 0.5.0 fait (base remise)" {
		t.Errorf("retour arrière non fait : %q", e.Message)
	}
}
