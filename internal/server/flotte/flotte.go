// SPDX-License-Identifier: GPL-3.0-or-later

// Package flotte : ce que le serveur offre aux machines en plus de la
// réception des données (SPEC.md § 16.7) :
//
//   - enrôlement (jeton à usage unique → « en attente » → approbation → token) ;
//   - configuration à distance (valeurs typées, ETag) ;
//   - battement (« je suis vivant », version, état d'une mise à jour) ;
//   - mise à jour : version assignée, manifeste signé, binaire.
//
// Rien n'est jamais poussé vers une machine : c'est toujours elle qui demande.
// Le serveur ne signe rien : il ne fait que distribuer ce que mirador-release a
// signé sur le poste de l'utilisateur.
package flotte

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/ingestion"
)

// Options de la flotte.
type Options struct {
	DB              *sql.DB
	Ingestion       *ingestion.Ingestion // vérification des tokens, débits
	DossierVersions string               // binaires publiés de l'agent
	DossierAgent    string               // binaires embarqués dans l'image (défaut DossierAgentEmbarque)
	Journal         *slog.Logger
	Maintenant      func() time.Time
}

// Flotte : points d'entrée des machines (hors métriques et logs).
type Flotte struct {
	opt Options

	demandes *ingestion.Limiteur // enrôlement : demandes par IP
	echecs   *ingestion.Limiteur // enrôlement : jetons faux par IP

	mu         sync.Mutex
	battements map[int64]battement // écrits en base une fois par minute
}

type battement struct {
	le int64
	b  protocole.Battement
}

// Nouvelle prépare la flotte.
func Nouvelle(opt Options) *Flotte {
	if opt.Journal == nil {
		opt.Journal = slog.New(slog.DiscardHandler)
	}
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	return &Flotte{
		opt: opt,
		// Un agent en attente redemande toutes les 30 s ; plusieurs machines
		// peuvent s'enrôler en même temps derrière un même NAT.
		demandes:   ingestion.NouveauLimiteur(1, 20, 10_000),
		echecs:     ingestion.NouveauLimiteur(0.1, 10, 10_000),
		battements: make(map[int64]battement),
	}
}

// Routes ajoute les points d'entrée au routeur des agents.
func (f *Flotte) Routes(m *http.ServeMux) {
	m.HandleFunc("POST "+protocole.CheminEnrolement, f.enroler)
	m.HandleFunc("GET "+protocole.CheminConfig, f.config)
	m.HandleFunc("POST "+protocole.CheminBattement, f.battre)
	m.HandleFunc("GET "+protocole.CheminMaj, f.maj)
	m.HandleFunc("GET "+protocole.CheminBinaire, f.binaire)
	m.HandleFunc("GET "+protocole.CheminInstallation, f.scriptInstallation)
	m.HandleFunc("GET "+protocole.CheminInstallationBinaire, f.binaireAInstaller)
	m.HandleFunc("GET "+protocole.CheminCles, f.cles)
}

// Boucle : écrit les battements en base une fois par minute, et à l'arrêt.
func (f *Flotte) Boucle(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fin, annuler := context.WithTimeout(context.Background(), 5*time.Second)
			defer annuler()
			if err := f.EcrireBattements(fin); err != nil {
				f.opt.Journal.Error("écriture des battements", "erreur", err)
			}
			return
		case <-t.C:
			if err := f.EcrireBattements(ctx); err != nil {
				f.opt.Journal.Error("écriture des battements", "erreur", err)
			}
		}
	}
}

// --- Aides HTTP --------------------------------------------------------------------

func repondreJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func erreur(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n") // #nosec G705 -- texte brut (nosniff), messages fixes
}

// lireJSON lit un petit document JSON, champs inconnus refusés.
func lireJSON(r *http.Request, v any) error {
	brut, err := io.ReadAll(io.LimitReader(r.Body, protocole.TailleMaxDemande+1))
	if err != nil {
		return err
	}
	if len(brut) > protocole.TailleMaxDemande {
		return errors.New("demande trop grosse")
	}
	dec := json.NewDecoder(bytes.NewReader(brut))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("contenu en trop")
	}
	return nil
}

// audit écrit une ligne du journal d'audit (jamais de secret).
func audit(ctx context.Context, x interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, le int64, qui, action, objet, avant, apres, adresse string) error {
	_, err := x.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		le, qui, action, objet, avant, apres, adresse)
	return err
}
