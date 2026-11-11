// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/godbus/dbus/v5"
)

// sourceDBus parle à systemd par le bus système, en lecture seule.
//
// Une seule connexion, gardée ouverte et refaite si elle tombe (redémarrage
// de dbus, par exemple).
type sourceDBus struct {
	mu   sync.Mutex
	conn *dbus.Conn
}

const (
	destSystemd = "org.freedesktop.systemd1"
	cheminMgr   = dbus.ObjectPath("/org/freedesktop/systemd1")
	ifaceMgr    = "org.freedesktop.systemd1.Manager"
	ifaceUnit   = "org.freedesktop.systemd1.Unit"
	ifaceSvc    = "org.freedesktop.systemd1.Service"
)

func (s *sourceDBus) connexion() (*dbus.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && s.conn.Connected() {
		return s.conn, nil
	}
	c, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	s.conn = c
	return c, nil
}

// Unites : un seul appel ListUnits pour toutes les unités chargées.
func (s *sourceDBus) Unites(ctx context.Context) ([]uniteEtat, error) {
	c, err := s.connexion()
	if err != nil {
		return nil, err
	}
	var brut []struct {
		Name, Description, LoadState, ActiveState, SubState, Followed string
		Path                                                          dbus.ObjectPath
		JobID                                                         uint32
		JobType                                                       string
		JobPath                                                       dbus.ObjectPath
	}
	if err := c.Object(destSystemd, cheminMgr).CallWithContext(ctx, ifaceMgr+".ListUnits", 0).Store(&brut); err != nil {
		return nil, err
	}
	out := make([]uniteEtat, 0, len(brut))
	for _, u := range brut {
		out = append(out, uniteEtat{Nom: u.Name, Etat: u.ActiveState, Charge: u.LoadState, Chemin: string(u.Path)})
	}
	return out, nil
}

// cheminUnite demande à systemd le chemin D-Bus d'une unité (et la charge si
// besoin, comme « systemctl show »).
func (s *sourceDBus) cheminUnite(ctx context.Context, c *dbus.Conn, nom string) (dbus.ObjectPath, error) {
	var p dbus.ObjectPath
	err := c.Object(destSystemd, cheminMgr).CallWithContext(ctx, ifaceMgr+".LoadUnit", 0, nom).Store(&p)
	return p, err
}

func (s *sourceDBus) Redemarrages(ctx context.Context, u uniteEtat) (uint32, error) {
	c, err := s.connexion()
	if err != nil {
		return 0, err
	}
	p := dbus.ObjectPath(u.Chemin)
	if !p.IsValid() {
		if p, err = s.cheminUnite(ctx, c, u.Nom); err != nil {
			return 0, err
		}
	}
	var v dbus.Variant
	err = c.Object(destSystemd, p).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, ifaceSvc, "NRestarts").Store(&v)
	if err != nil {
		return 0, err
	}
	n, ok := v.Value().(uint32)
	if !ok {
		return 0, errors.New("NRestarts : type inattendu")
	}
	return n, nil
}

func (s *sourceDBus) ServicesActivesAuDemarrage(ctx context.Context) ([]string, error) {
	c, err := s.connexion()
	if err != nil {
		return nil, err
	}
	var brut []struct {
		Path, State string
	}
	err = c.Object(destSystemd, cheminMgr).CallWithContext(ctx, ifaceMgr+".ListUnitFilesByPatterns", 0,
		[]string{"enabled"}, []string{"*.service"}).Store(&brut)
	if err != nil {
		return nil, err
	}
	noms := make([]string, 0, len(brut))
	for _, f := range brut {
		noms = append(noms, filepath.Base(f.Path))
	}
	return noms, nil
}

func (s *sourceDBus) Proprietes(ctx context.Context, service string) (proprietesService, error) {
	var p proprietesService
	c, err := s.connexion()
	if err != nil {
		return p, err
	}
	chemin, err := s.cheminUnite(ctx, c, service)
	if err != nil {
		return p, err
	}
	obj := c.Object(destSystemd, chemin)
	var unite, svc map[string]dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, ifaceUnit).Store(&unite); err != nil {
		return p, err
	}
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, ifaceSvc).Store(&svc); err != nil {
		return p, err
	}
	p.Etat, _ = unite["ActiveState"].Value().(string)
	p.ConditionOK, _ = unite["ConditionResult"].Value().(bool)
	if t, ok := unite["TriggeredBy"].Value().([]string); ok {
		p.DeclencheePar = len(t)
	}
	p.Type, _ = svc["Type"].Value().(string)
	p.CodeSortie, _ = svc["ExecMainCode"].Value().(int32)
	p.StatutSortie, _ = svc["ExecMainStatus"].Value().(int32)
	p.DebutMono, _ = svc["ExecMainStartTimestampMonotonic"].Value().(uint64)
	p.FinMono, _ = svc["ExecMainExitTimestampMonotonic"].Value().(uint64)
	return p, nil
}
