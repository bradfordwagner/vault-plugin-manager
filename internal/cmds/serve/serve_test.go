package serve

import (
	"strings"
	"testing"

	"vault-plugin-manager/internal/args"
)

func TestValidateRequiresStateSecretName(t *testing.T) {
	a := args.ServeArgs{VaultAddr: "https://vault:8200", VaultAuthRole: "vpm", ConfigMapName: "vpm"}
	err := validate(a)
	if err == nil || !strings.Contains(err.Error(), "state_secret_name") {
		t.Fatalf("validate = %v, want an error naming state_secret_name", err)
	}
	a.StateSecretName = "vpm-state"
	if err := validate(a); err != nil {
		t.Fatalf("validate with every required field = %v", err)
	}
}
