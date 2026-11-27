// SPDX-License-Identifier: GPL-3.0-or-later

package base

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Migration 0040 (retour du 27/11) : les adresses « C'est moi » de la 0.6.32
// deviennent des plages /32 ou /128 pour toutes les machines.
func TestMigrationSSHConnues(t *testing.T) {
	ctx := context.Background()
	db, err := Ouvrir(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ancien, _ := os.ReadFile(filepath.Join("migrations", "0038_ssh.sql"))
	nouveau, _ := os.ReadFile(filepath.Join("migrations", "0040_ssh_connues_plages.sql"))
	if _, err := db.ExecContext(ctx, string(ancien)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ssh_connues VALUES ('198.51.100.23', 'admin', 1), ('2001:db8::7', 'admin', 2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(nouveau)); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT prefixe, machine, par FROM ssh_connues ORDER BY le`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p, m, par string
		_ = rows.Scan(&p, &m, &par)
		got = append(got, p+"|"+m+"|"+par)
	}
	if len(got) != 2 || got[0] != "198.51.100.23/32||admin" || got[1] != "2001:db8::7/128||admin" {
		t.Fatalf("après migration : %v", got)
	}
}
