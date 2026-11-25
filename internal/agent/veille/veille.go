// SPDX-License-Identifier: GPL-3.0-or-later

// Package veille : pannes vues tout de suite par l'agent (SPEC § 98).
//
// Sans attendre le relevé suivant (30 s) :
//   - un service systemd qui passe en échec : signal D-Bus de systemd
//     (PropertiesChanged, ActiveState = failed), en lecture seule ;
//   - le même service qui sort de l'échec (actif, ou arrêté proprement) : le
//     retour est vu aussi vite que la panne (SPEC § 127) ;
//   - un processus tué faute de mémoire : compteur oom_kill de /proc/vmstat,
//     relu toutes les 2 s (une lecture de fichier, ~20 µs).
//
// À chaque panne, Signaler est appelé : l'agent envoie ses métriques tout de
// suite et prévient le serveur. Aucune commande n'est lancée.
package veille

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"mirador-stack/internal/protocole"
)

// Veilleur surveille les pannes.
type Veilleur struct {
	Proc string // /proc (remplacé dans les tests)
	// Systemd : écouter systemd (collecteur systemd activé) ; Garder : services
	// suivis (nil : tous les .service ; validation un par un sinon).
	Systemd  bool
	Garder   func(unite string) bool
	Signaler func(protocole.EvenementAgent)
	Journal  *slog.Logger

	mu    sync.Mutex
	vues  map[string]time.Time // unité → dernier signalement (pas deux fois par minute)
	echec map[string]bool      // unités vues en échec (leur retour est signalé)
}

const (
	repriseDBus  = 30 * time.Second // connexion perdue : nouvel essai
	memeUnite    = time.Minute      // une unité en boucle n'est signalée qu'une fois par minute
	ifaceProps   = "org.freedesktop.DBus.Properties"
	cheminUnites = dbus.ObjectPath("/org/freedesktop/systemd1/unit")
)

// rythmeOOM : relecture du compteur oom_kill (remplacé dans les tests).
var rythmeOOM = 2 * time.Second

// Tourner surveille jusqu'à l'arrêt de ctx.
func (v *Veilleur) Tourner(ctx context.Context) {
	if v.Journal == nil {
		v.Journal = slog.New(slog.DiscardHandler)
	}
	if v.Proc == "" {
		v.Proc = "/proc"
	}
	if v.Systemd {
		go v.ecouterSystemd(ctx)
	}
	v.surveillerOOM(ctx)
}

// --- Manque de mémoire ------------------------------------------------------------------

func lireOOM(proc string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(proc, "vmstat")) // #nosec G304 -- /proc/vmstat (chemin figé, remplacé seulement dans les tests)
	if err != nil {
		return 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "oom_kill "); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func (v *Veilleur) surveillerOOM(ctx context.Context) {
	avant, ok := lireOOM(v.Proc)
	if !ok {
		return // noyau sans ce compteur (très ancien) : rien à surveiller
	}
	t := time.NewTicker(rythmeOOM)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, ok := lireOOM(v.Proc)
		if ok && n > avant {
			texte := strconv.FormatUint(n-avant, 10) + " processus tué(s)"
			v.Signaler(protocole.EvenementAgent{Type: protocole.EvenementOOM, Texte: texte, Le: time.Now().UnixMilli()})
		}
		if ok {
			avant = n
		}
	}
}

// --- systemd ----------------------------------------------------------------------------

// nomUnite : « /org/freedesktop/systemd1/unit/nginx_2eservice » → « nginx.service »
// (systemd remplace chaque caractère hors [A-Za-z0-9] par _XX en hexadécimal).
func nomUnite(p dbus.ObjectPath) string {
	s, ok := strings.CutPrefix(string(p), string(cheminUnites)+"/")
	if !ok || strings.Contains(s, "/") {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '_' && i+2 < len(s) {
			if c, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(c))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// suivie : unité .service que le collecteur systemd surveille.
func (v *Veilleur) suivie(unite string) bool {
	return strings.HasSuffix(unite, ".service") && (v.Garder == nil || v.Garder(unite))
}

// aSignaler : unité suivie, pas déjà signalée dans la minute (cle : l'unité
// pour une panne, « retour: » + l'unité pour un retour : un service réparé
// juste après sa panne est quand même signalé).
func (v *Veilleur) aSignaler(cle, unite string, t time.Time) bool {
	if !v.suivie(unite) {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.vues == nil {
		v.vues = map[string]time.Time{}
	}
	if d, ok := v.vues[cle]; ok && t.Sub(d) < memeUnite {
		return false
	}
	if len(v.vues) > 1000 { // borne mémoire
		v.vues = map[string]time.Time{}
	}
	v.vues[cle] = t
	return true
}

// noterEchec : retient (ou oublie) une unité en échec ; renvoie vrai si
// l'unité était en échec avant (son retour est à signaler).
func (v *Veilleur) noterEchec(unite string, enEchec bool) (avant bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.echec == nil {
		v.echec = map[string]bool{}
	}
	avant = v.echec[unite]
	switch {
	case enEchec && len(v.echec) < 1000: // borne mémoire
		v.echec[unite] = true
	case !enEchec:
		delete(v.echec, unite)
	}
	return avant
}

// traiter : un signal PropertiesChanged d'une unité.
func (v *Veilleur) traiter(s *dbus.Signal) {
	if s.Name != ifaceProps+".PropertiesChanged" || len(s.Body) < 2 {
		return
	}
	iface, _ := s.Body[0].(string)
	props, _ := s.Body[1].(map[string]dbus.Variant)
	if iface != "org.freedesktop.systemd1.Unit" || props == nil {
		return
	}
	etat, ok := props["ActiveState"]
	if !ok {
		return
	}
	unite := nomUnite(s.Path)
	if unite == "" || !v.suivie(unite) {
		return
	}
	maintenant := time.Now()
	switch e, _ := etat.Value().(string); e {
	case "failed":
		v.noterEchec(unite, true)
		if v.aSignaler(unite, unite, maintenant) {
			v.Signaler(protocole.EvenementAgent{Type: protocole.EvenementService, Objet: unite, Texte: "passé en échec", Le: maintenant.UnixMilli()})
		}
	case "active", "inactive":
		// Sorti de l'échec : relancé (actif) ou remis à zéro / arrêté
		// proprement (inactif). Les états de passage (activating…) attendent.
		if v.noterEchec(unite, false) && v.aSignaler("retour:"+unite, unite, maintenant) {
			texte := "de nouveau actif"
			if e == "inactive" {
				texte = "sorti de l'échec (arrêté)"
			}
			v.Signaler(protocole.EvenementAgent{Type: protocole.EvenementServiceRetour, Objet: unite, Texte: texte, Le: maintenant.UnixMilli()})
		}
	}
}

// uniteEchouee : une ligne de ListUnitsFiltered (nom, description, états…).
type uniteEchouee struct {
	Nom, Description, Chargement, Actif, Sous, Suit string
	Chemin                                          dbus.ObjectPath
	Tache                                           uint32
	TypeTache                                       string
	CheminTache                                     dbus.ObjectPath
}

// lireEchecs : unités déjà en échec au démarrage de l'écoute (lecture seule) :
// leur retour sera signalé aussi.
func (v *Veilleur) lireEchecs(ctx context.Context, c *dbus.Conn) {
	var unites []uniteEchouee
	if err := c.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").CallWithContext(ctx,
		"org.freedesktop.systemd1.Manager.ListUnitsFiltered", 0, []string{"failed"}).Store(&unites); err != nil {
		v.Journal.Debug("liste des unités en échec illisible", "erreur", err)
		return
	}
	for _, u := range unites {
		if v.suivie(u.Nom) {
			v.noterEchec(u.Nom, true)
		}
	}
}

// ecouterSystemd : abonnement aux changements d'état des unités (lecture
// seule ; « Subscribe » est permis à tout utilisateur), repris si la
// connexion tombe.
func (v *Veilleur) ecouterSystemd(ctx context.Context) {
	for {
		if err := v.uneEcoute(ctx); err != nil {
			v.Journal.Debug("veille systemd interrompue", "erreur", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(repriseDBus):
		}
	}
}

func (v *Veilleur) uneEcoute(ctx context.Context) error {
	c, err := dbus.ConnectSystemBus()
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.AddMatchSignalContext(ctx, dbus.WithMatchInterface(ifaceProps), dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchPathNamespace(cheminUnites)); err != nil {
		return err
	}
	if err := c.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1").CallWithContext(ctx,
		"org.freedesktop.systemd1.Manager.Subscribe", 0).Err; err != nil {
		return err
	}
	ch := make(chan *dbus.Signal, 64)
	c.Signal(ch)
	v.lireEchecs(ctx, c)
	for {
		select {
		case <-ctx.Done():
			return nil
		case s, ok := <-ch:
			if !ok {
				return nil // connexion fermée
			}
			v.traiter(s)
		}
	}
}
