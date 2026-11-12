// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/version"
)

// Lien : tout ce que l'agent demande à mirador-server en plus de l'envoi des
// données (SPEC.md § 16.7). C'est toujours l'agent qui demande ; le serveur ne
// peut que répondre, avec des valeurs typées.
//
//   - battement toutes les minutes (version, démarrage, état d'une mise à jour) ;
//   - configuration toutes les 5 min (ETag) : si elle change, elle est gardée
//     sur disque et l'agent redémarre proprement pour l'appliquer ;
//   - mise à jour (si MIRADOR_MAJ_AUTO=oui) toutes les 15 min : manifeste
//     vérifié AVANT le téléchargement, binaire vérifié, self-test, bascule,
//     redémarrage ; une version qui a déjà échoué n'est pas retentée ;
//   - après une mise à jour : sans battement réussi en 10 min, retour à la
//     version précédente.
type Lien struct {
	Cfg          config.Agent // configuration en cours (locale + distante)
	Locale       config.Agent // configuration du seul fichier local
	Token        string
	Client       *http.Client
	Journal      *slog.Logger
	Empreinte    string                 // empreinte de la machine ('' : pas envoyée)
	Installateur *majagent.Installateur // nil : pas de mise à jour
	// Redemarrer arrête l'agent proprement ; systemd le relance.
	Redemarrer func(raison string)

	demarrage time.Time
	mu        sync.Mutex
	echecMaj  string // dernier échec de mise à jour (signalé au serveur)
	confirme  chan struct{}
	unSucces  sync.Once

	// Rythmes (modifiables dans les tests).
	rythmeBattement, rythmeConfig, rythmeMaj, delaiEssai, premierDelai time.Duration
}

// ReglerRythmes : pour les tests seulement (tout à 0 = rythmes normaux).
func (l *Lien) ReglerRythmes(battement, config, maj, essai, premier time.Duration) {
	l.rythmeBattement, l.rythmeConfig, l.rythmeMaj, l.delaiEssai, l.premierDelai = battement, config, maj, essai, premier
}

func (l *Lien) url(chemin string) string { return l.Cfg.Serveur + chemin }

func alea(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) // #nosec G404 -- étalement des requêtes, pas de sécurité
}

// Tourner lance les boucles jusqu'à l'arrêt du contexte.
func (l *Lien) Tourner(ctx context.Context) {
	l.demarrage = time.Now()
	l.confirme = make(chan struct{})
	if l.rythmeBattement == 0 {
		// Premières demandes étalées : 25 agents ne demandent pas tous en même temps.
		l.rythmeBattement, l.rythmeConfig, l.rythmeMaj, l.delaiEssai, l.premierDelai =
			time.Minute, 5*time.Minute, 15*time.Minute, 10*time.Minute, time.Minute
	}
	var wg sync.WaitGroup
	lancer := func(f func(context.Context)) {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	lancer(l.boucleBattement)
	lancer(l.boucleConfig)
	if l.Installateur != nil {
		lancer(l.surveillerEssai)
		if l.Cfg.MajAuto {
			lancer(l.boucleMaj)
		}
	}
	wg.Wait()
}

// etale : délai aléatoire entre 0 et premierDelai.
func (l *Lien) etale() time.Duration {
	return time.Duration(rand.Int64N(int64(l.premierDelai) + 1)) // #nosec G404 -- étalement, pas de sécurité
}

func attendre(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// requete : appel authentifié, réponse lue au plus max octets.
func (l *Lien) requete(ctx context.Context, methode, chemin string, corps []byte, entetes map[string]string, max int64) (*http.Response, []byte, error) {
	ctx, annuler := context.WithTimeout(ctx, 5*time.Minute)
	defer annuler()
	var r io.Reader
	if corps != nil {
		r = bytes.NewReader(corps)
	}
	req, err := http.NewRequestWithContext(ctx, methode, l.url(chemin), r)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	req.Header.Set(protocole.EnteteProtocole, "1")
	req.Header.Set(protocole.EnteteVersion, version.Version)
	req.Header.Set(protocole.EnteteHeure, fmt.Sprint(time.Now().UnixMilli()))
	req.Header.Set(protocole.EnteteCapacites, protocole.CapaciteSurveillance)
	if l.Empreinte != "" {
		req.Header.Set(protocole.EnteteEmpreinte, l.Empreinte)
	}
	req.Header.Set("User-Agent", "mirador-agent/"+version.Version)
	for k, v := range entetes {
		req.Header.Set(k, v)
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	brut, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return resp, nil, err
	}
	if int64(len(brut)) > max {
		return resp, nil, errors.New("réponse trop grosse")
	}
	return resp, brut, nil
}

// --- Battement ---------------------------------------------------------------------

func (l *Lien) boucleBattement(ctx context.Context) {
	for {
		if err := l.battre(ctx); err != nil {
			l.Journal.Debug("battement en échec", "erreur", err)
		}
		if !attendre(ctx, alea(l.rythmeBattement)) {
			return
		}
	}
}

func (l *Lien) battre(ctx context.Context) error {
	b := protocole.Battement{Version: version.Version, Architecture: runtime.GOARCH, Demarrage: l.demarrage}
	if l.Installateur != nil {
		if e, _ := l.Installateur.EnEssai(); e != nil {
			b.MajEssai = e.Nouveau
		}
		if v, raison := l.Installateur.DernierEchec(); v != "" {
			b.MajEchec = tronquer(v+" : "+raison, 200)
		}
	}
	l.mu.Lock()
	if l.echecMaj != "" {
		b.MajEchec = tronquer(l.echecMaj, 200)
	}
	l.mu.Unlock()
	corps, err := json.Marshal(b)
	if err != nil {
		return err
	}
	resp, _, err := l.requete(ctx, http.MethodPost, protocole.CheminBattement, corps,
		map[string]string{"Content-Type": "application/json"}, 4<<10)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	l.unSucces.Do(func() { close(l.confirme) })
	return nil
}

// tronquer garde les n premiers octets en ne coupant pas un caractère, et ne
// garde que les caractères acceptés par le serveur.
func tronquer(s string, n int) string {
	var out []rune
	taille := 0
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(" .,:;/()'+_-", r) {
			r = ' ' // même liste que le serveur : le battement ne sera pas refusé
		}
		if taille += len(string(r)); taille > n {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// surveillerEssai : après une mise à jour, le nouvel agent doit joindre le
// serveur dans le délai ; sinon, retour à la version précédente.
func (l *Lien) surveillerEssai(ctx context.Context) {
	e, err := l.Installateur.EnEssai()
	if err != nil || e == nil {
		return
	}
	l.Journal.Info("nouvelle version en essai", "version", e.Nouveau, "delai", l.delaiEssai.String())
	select {
	case <-ctx.Done():
	case <-l.confirme:
		if err := l.Installateur.Confirmer(); err != nil {
			l.Journal.Error("confirmation de la mise à jour", "erreur", err)
			return
		}
		l.Journal.Info("mise à jour confirmée", "version", e.Nouveau)
	case <-time.After(l.delaiEssai):
		if err := l.Installateur.RetourArriere("serveur injoignable après la mise à jour"); err != nil {
			l.Journal.Error("retour arrière impossible", "erreur", err)
			return
		}
		l.Journal.Warn("pas de contact avec le serveur : retour à la version précédente", "version", e.Precedent)
		l.Redemarrer("retour arrière")
	}
}

// --- Configuration distante --------------------------------------------------------

func (l *Lien) boucleConfig(ctx context.Context) {
	r := &Recuperateur{URL: l.url(protocole.CheminConfig), Token: l.Token, Client: l.Client, Empreinte: l.Empreinte}
	if !attendre(ctx, l.etale()) {
		return
	}
	for {
		rythme := l.rythmeConfig
		d, err := r.Recuperer(ctx)
		switch {
		case errors.Is(err, ErrInchange):
		case err != nil:
			l.Journal.Warn("configuration distante non récupérée", "erreur", err)
		default:
			if d.Recuperation != nil {
				rythme = time.Duration(*d.Recuperation)
			}
			l.appliquer(d)
		}
		if !attendre(ctx, alea(rythme)) {
			return
		}
	}
}

// appliquer : une configuration refusée est ignorée (la configuration en cours
// est gardée) ; une configuration différente est enregistrée puis l'agent
// redémarre pour l'appliquer à tous ses collecteurs.
func (l *Lien) appliquer(d *ConfigDistante) {
	nouvelle, err := Appliquer(l.Locale, d)
	if err != nil {
		l.Journal.Error("configuration distante refusée, la configuration actuelle est gardée", "erreur", err)
		return
	}
	if reflect.DeepEqual(nouvelle, l.Cfg) {
		return
	}
	if err := EnregistrerConfig(l.Cfg.FichierConfigDistante, d); err != nil {
		l.Journal.Error("enregistrement de la configuration distante", "erreur", err)
		return
	}
	l.Journal.Info("configuration distante modifiée : redémarrage pour l'appliquer")
	l.Redemarrer("configuration modifiée")
}

// EnregistrerConfig garde le document reçu (0600, écriture atomique).
func EnregistrerConfig(chemin string, d *ConfigDistante) error {
	brut, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(chemin), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(brut); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), chemin)
}

// ConfigAuDemarrage : configuration locale complétée par la dernière
// configuration distante gardée sur disque (si elle est valable).
func ConfigAuDemarrage(locale config.Agent, journal *slog.Logger) config.Agent {
	if locale.Serveur == "" || locale.FichierConfigDistante == "" {
		return locale
	}
	f, err := os.Open(locale.FichierConfigDistante)
	if err != nil {
		return locale
	}
	defer f.Close()
	d, err := Decoder(f)
	if err == nil {
		var a config.Agent
		if a, err = Appliquer(locale, d); err == nil {
			return a
		}
	}
	journal.Warn("configuration distante gardée illisible ou refusée : configuration locale seule", "erreur", err)
	return locale
}

// --- Mise à jour -------------------------------------------------------------------

func (l *Lien) boucleMaj(ctx context.Context) {
	if !attendre(ctx, l.etale()+l.premierDelai) {
		return
	}
	for {
		if err := l.mettreAJour(ctx); err != nil {
			l.mu.Lock()
			l.echecMaj = err.Error()
			l.mu.Unlock()
			l.Journal.Error("mise à jour impossible", "erreur", err)
		}
		if !attendre(ctx, alea(l.rythmeMaj)) {
			return
		}
	}
}

func (l *Lien) mettreAJour(ctx context.Context) error {
	if e, _ := l.Installateur.EnEssai(); e != nil {
		return nil // un essai est déjà en cours
	}
	resp, brut, err := l.requete(ctx, http.MethodGet, protocole.CheminMaj, nil, nil, 16<<10)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("version assignée : HTTP %d", resp.StatusCode)
	}
	var rep protocole.ReponseMaj
	dec := json.NewDecoder(bytes.NewReader(brut))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return fmt.Errorf("réponse illisible : %w", err)
	}
	m := rep.Manifeste
	if m == nil || m.Version == version.Version {
		return nil
	}
	if v, _ := l.Installateur.DernierEchec(); v == m.Version {
		return nil // déjà essayée sans succès : attendre une autre version
	}
	// Signature, cohérence et anti-retour arrière vérifiés AVANT de télécharger,
	// avec les clés posées à l'installation (fichier root, relu à chaque fois :
	// une rotation faite par l'administrateur est vue sans redémarrage).
	cles, err := majagent.ClesInstallees(l.Locale.FichierCles)
	if err != nil {
		return fmt.Errorf("version %s refusée : clés de signature : %w", m.Version, err)
	}
	if err := majagent.VerifierManifeste(m, version.Version, cles); err != nil {
		return fmt.Errorf("version %s refusée : %w", m.Version, err)
	}
	q := url.Values{protocole.ParametreVersion: {m.Version}, protocole.ParametreArchitecture: {m.Architecture}}
	resp, binaire, err := l.requete(ctx, http.MethodGet, protocole.CheminBinaire+"?"+q.Encode(), nil, nil, majagent.TailleMax)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("binaire %s : HTTP %d", m.Version, resp.StatusCode)
	}
	if err := l.Installateur.Installer(ctx, m, binaire); err != nil {
		return fmt.Errorf("installation de %s : %w", m.Version, err)
	}
	l.Journal.Info("nouvelle version installée : redémarrage", "version", m.Version)
	l.Redemarrer("mise à jour " + m.Version)
	return nil
}
