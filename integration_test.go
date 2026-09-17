package egtest_test

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/dio/egtest"
)

//go:embed testdata/helm-values.yaml
var helmValues []byte

func TestInstallLive(t *testing.T) {
	if os.Getenv("EGTEST_INTEGRATION") != "1" {
		t.Skip("not run: invoke make integration with explicit version pins")
	}
	var c *egtest.Cluster
	var info egtest.Info
	t.Cleanup(func() {
		if c != nil {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
			info = c.Info()
		}
		if path := os.Getenv("EGTEST_REPORT"); path != "" {
			raw, err := json.MarshalIndent(struct {
				Info   egtest.Info `json:"info"`
				Passed bool        `json:"passed"`
			}{info, !t.Failed()}, "", "  ")
			if err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
				t.Error(err)
			}
		}
	})
	var err error
	c, err = egtest.Open(t.Context(), egtest.Options{
		EGVersion:  os.Getenv("EGTEST_EG_VERSION"),
		K3SVersion: os.Getenv("EGTEST_K3S_VERSION"),
		HelmValues: helmValues,
	})
	if err != nil {
		var setup *egtest.SetupError
		if errors.As(err, &setup) {
			info = setup.Info
		}
		t.Fatal(err)
	}
	if !c.Info().InstallationVerified {
		t.Fatal("installation not verified")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Info().Cleanup != "verified" {
		t.Fatal("cleanup not verified")
	}
}
