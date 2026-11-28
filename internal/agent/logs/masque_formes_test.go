// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import "testing"

// TestMasquageFormes : formes de secrets ajoutées à la revue du 28/11
// (JSON échappé, options de commande, adresse avec identifiants, valeur
// entre guillemets avec espaces, cookies), et faux positifs à éviter.
func TestMasquageFormes(t *testing.T) {
	m, err := NewMasqueur(ModelesPlancher, nil)
	if err != nil {
		t.Fatal(err)
	}
	cas := []struct{ entree, attendu string }{
		// JSON échappé (message JSON rangé dans un champ texte).
		{`msg="{\"user\":\"bob\",\"password\":\"x\"}"`, `msg="{\"user\":\"bob\",\"password\":\"***\"}"`},
		{`{\"password\": \"a b c\", \"ok\":1}`, `{\"password\": \"***\", \"ok\":1}`},
		{`{\"access_token\":\"eyJ.abc\"}`, `{\"access_token\":\"***\"}`},
		{`{\"Authorization\":\"Bearer eyJ.abc\"}`, `{\"Authorization\":\"Bearer ***\"}`},
		// Valeur entre guillemets avec espaces : masquée en entier.
		{`login password="mon mot de passe" ok`, `login password="***" ok`},
		{`{"password": "Sup3r S3cret", "user":"bob"}`, `{"password": "***", "user":"bob"}`},
		{`secret='deux mots' fin`, `secret='***' fin`},
		{`password=""`, `password=""`},
		// Options de ligne de commande.
		{"CMD (/usr/bin/backup --password S3cret --verbose)", "CMD (/usr/bin/backup --password *** --verbose)"},
		{"tool --password=S3cret --x", "tool --password=*** --x"},
		{"tool --pass S3cret", "tool --pass ***"},
		{`tool --password "a b" -v`, `tool --password "***" -v`},
		{"tool --password --verbose", "tool --password --verbose"}, // pas de valeur
		{"mysqldump -uroot -pS3cret base", "mysqldump -uroot -p*** base"},
		{"CMD (mysql -h db -u app -pS3cret -e 'select 1')", "CMD (mysql -h db -u app -p*** -e 'select 1')"},
		{"mysql -u root -p base", "mysql -u root -p base"},     // mot de passe demandé
		{"mysql -h db -P3306 base", "mysql -h db -P3306 base"}, // port, pas mot de passe
		{"ssh -p2222 admin@hote", "ssh -p2222 admin@hote"},     // pas mysql
		{"mkdir -p /var/lib/x --parents", "mkdir -p /var/lib/x --parents"},
		{"mount --passive x", "mount --passive x"},
		// Identifiants dans une adresse.
		{"connexion postgres://app:motdepasse@db.lan:5432/app", "connexion postgres://app:***@db.lan:5432/app"},
		{"REDIS_URL=redis://:s3cret@cache:6379/0", "REDIS_URL=redis://:***@cache:6379/0"},
		{"https://user:p@ss@exemple.fr/x", "https://user:***@exemple.fr/x"},
		{"GET https://exemple.fr:8443/a@b", "GET https://exemple.fr:8443/a@b"}, // pas d'identifiants
		{"vers http://10.0.0.1:8080/ ok", "vers http://10.0.0.1:8080/ ok"},
		// Cookies.
		{"Cookie: sid=abc123; theme=sombre", "Cookie: ***"},
		{"Set-Cookie: session=xyz; Path=/; HttpOnly", "Set-Cookie: ***"},
		{`{"cookie":"sid=abc; a=b","path":"/"}`, `{"cookie":"***","path":"/"}`},
		{`{\"Cookie\":\"sid=abc\"}`, `{\"Cookie\":\"***\"}`},
		// Formes déjà couvertes (non-régression).
		{"user=bob password=Sup3rS3cret! from 10.0.0.1", "user=bob password=*** from 10.0.0.1"},
		{"GET /api?token=eyJ.abc&x=1", "GET /api?token=***&x=1"},
		{"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.e30.abc", "Authorization: Bearer ***"},
		{`password=a\b\c fin`, `password=*** fin`}, // barre oblique inverse dans la valeur
		{"Failed password for root from 10.0.0.1", "Failed password for root from 10.0.0.1"},
		{"Accepted publickey for admin", "Accepted publickey for admin"},
	}
	for _, c := range cas {
		if obtenu := m.Masquer(c.entree); obtenu != c.attendu {
			t.Errorf("Masquer(%q)\n  = %q\n  attendu %q", c.entree, obtenu, c.attendu)
		}
	}
}
