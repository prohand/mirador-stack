// SPDX-License-Identifier: GPL-3.0-or-later

package relance

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Le programme se remplace par lui-même : même PID, mêmes arguments.
func TestExecuter(t *testing.T) {
	switch os.Getenv("RELANCE_ETAPE") {
	case "1":
		_ = os.Setenv("RELANCE_ETAPE", "2")
		_ = os.Setenv("RELANCE_PID", fmt.Sprint(os.Getpid()))
		err := Executer()
		fmt.Println("échec :", err)
		os.Exit(1)
	case "2":
		fmt.Println("relancé", os.Getenv("RELANCE_PID") == fmt.Sprint(os.Getpid()))
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecuter$") // #nosec G204 -- le binaire de test lui-même
	cmd.Env = append(os.Environ(), "RELANCE_ETAPE=1")
	sortie, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(sortie), "relancé true") {
		t.Fatalf("%v : %s", err, sortie)
	}
}
